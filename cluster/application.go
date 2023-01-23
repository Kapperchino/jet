package cluster

import (
	"context"
	fsmPb "github.com/Kapperchino/jet-application/proto"
	pb "github.com/Kapperchino/jet-cluster/proto"
	"github.com/Kapperchino/jet/util"
	"github.com/alphadose/haxmap"
	"github.com/hashicorp/memberlist"
	"github.com/hashicorp/raft"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"time"
)

type RpcInterface struct {
	Raft         *raft.Raft
	ClusterState *ClusterState
	Logger       *zerolog.Logger
	MemberList   *memberlist.Memberlist
	pb.UnimplementedClusterMetaServiceServer
}

type ClusterState struct {
	ShardId       string
	CurShardState *ShardState
	ClusterInfo   *haxmap.Map[string, *ShardInfo]
}

type ShardState struct {
	ShardInfo  *ShardInfo
	MemberInfo *MemberInfo
	RaftChan   chan raft.Observation
}

type MemberInfo struct {
	NodeId   string
	IsLeader bool
	Address  string
}

type ShardInfo struct {
	shardId   string
	leader    string
	MemberMap *haxmap.Map[string, MemberInfo]
}

func InitClusterState(i *RpcInterface, nodeName string, address string, shardId string) *ClusterState {
	clusterState := &ClusterState{
		ClusterInfo: haxmap.New[string, *ShardInfo](),
		CurShardState: &ShardState{
			RaftChan: make(chan raft.Observation, 50),
			ShardInfo: &ShardInfo{
				shardId:   shardId,
				leader:    "",
				MemberMap: haxmap.New[string, MemberInfo](),
			},
			MemberInfo: &MemberInfo{
				NodeId:   nodeName,
				IsLeader: false,
				Address:  address,
			},
		},
	}
	clusterState.CurShardState.ShardInfo.MemberMap.Set(nodeName, MemberInfo{
		NodeId:   nodeName,
		IsLeader: false,
		Address:  address,
	})
	clusterState.ClusterInfo.Set(shardId, clusterState.CurShardState.ShardInfo)
	observer := raft.NewObserver(clusterState.CurShardState.RaftChan, false, nil)
	i.Raft.RegisterObserver(observer)
	go onRaftUpdates(clusterState.CurShardState.RaftChan, i)
	return clusterState
}

func InitClusterListener(clusterState *ClusterState) *ClusterListener {
	listener := ClusterListener{
		state: clusterState,
	}
	return &listener
}

func onRaftUpdates(raftChan chan raft.Observation, i *RpcInterface) {
	for {
		time.Sleep(time.Second * 1)
		select {
		case observable := <-raftChan:
			switch val := observable.Data.(type) {
			case raft.RequestVoteRequest:
				i.Logger.Debug().Msg("vote requested")
				break
			case raft.RaftState:
				onStateUpdate(i, val)
				break
			case raft.PeerObservation:
				onPeerUpdate(i, val)
				break
			case raft.LeaderObservation:
				onLeaderUpdate(i, val)
				break
			case raft.FailedHeartbeatObservation:
				onFailedHeartbeat(i, val)
				break
			}
		default:
			i.Logger.Trace().Msgf("No updates for shard %s", i.getShardId())
		}
	}

}
func onFailedHeartbeat(i *RpcInterface, update raft.FailedHeartbeatObservation) {
	i.Logger.Warn().Msgf("Peer %s cannot be connected to, last contact: %s, nodes: %s", update.PeerID, update.LastContact.String(), i.Raft.GetConfiguration().Configuration().Servers)
	if time.Now().Sub(update.LastContact).Seconds() > 10 {
		err := i.Raft.RemoveServer(update.PeerID, i.Raft.LastIndex()-1, 0).Error()
		if err != nil {
			return
		}
		i.Logger.Info().Msgf("Peer %s is removed", update.PeerID)
		i.getMemberMap().Del(string(update.PeerID))
		err = RemovePeer(i, string(update.PeerID))
		if err != nil {
			log.Err(err).Msgf("Error removing peer %s", update.PeerID)
			return
		}
	}
}

func onPeerUpdate(i *RpcInterface, update raft.PeerObservation) {
	if update.Removed {
		i.Logger.Info().Msgf("Peer %s is removed", update.Peer.ID)
		i.getMemberMap().Del(string(update.Peer.ID))
		err := RemovePeer(i, string(update.Peer.ID))
		if err != nil {
			log.Err(err).Msgf("Error removing peer %s", update.Peer.ID)
			return
		}
		return
	}
	if len(update.Peer.ID) == 0 {
		return
	}
	//peer is updated
	_, exist := i.getMemberMap().Get(string(update.Peer.ID))
	if exist {
		i.Logger.Debug().Msgf("Peer %s already exists, no-op", update.Peer.ID)
		return
	}
	//add peer
	i.getMemberMap().Set(string(update.Peer.ID), MemberInfo{
		NodeId:   string(update.Peer.ID),
		IsLeader: false,
		Address:  string(update.Peer.Address),
	})

	i.Logger.Info().Msgf("Replicating peer %s", update.Peer.ID)
	err := ReplicatePeer(i, update)
	if err != nil {
		log.Err(err).Msgf("Error replicating peer %s", update.Peer.ID)
		return
	}
	i.Logger.Info().Msgf("Peer %s is added", update.Peer.ID)
}

func onLeaderUpdate(i *RpcInterface, update raft.LeaderObservation) {
	if string(update.LeaderID) == i.getMemberInfo().NodeId {
		i.Logger.Debug().Msg("Node is already leader")
		i.getCurShardInfo().leader = string(update.LeaderID)
		i.getMemberInfo().IsLeader = true
		return
	}
	//leadership update
	i.getMemberInfo().IsLeader = false
	i.getCurShardInfo().leader = string(update.LeaderID)
	_, exist := i.getMemberMap().Get(string(update.LeaderID))
	if exist {
		i.Logger.Debug().Msgf("Leader %s already exists, no-op", update.LeaderAddr)
		return
	}
	//leader added, we should try to see if we can get a list of servers to add to
	servers := i.Raft.GetConfiguration().Configuration().Servers
	for _, server := range servers {
		_, exist := i.getMemberMap().Get(string(server.ID))
		if !exist {
			i.Logger.Info().Msgf("adding server %s", server)
			i.getMemberMap().Set(string(server.ID), MemberInfo{
				NodeId:   string(server.ID),
				IsLeader: false,
				Address:  string(server.Address),
			})
		}
	}
	i.getMemberMap().Set(string(update.LeaderID), MemberInfo{
		NodeId:   string(update.LeaderID),
		IsLeader: true,
		Address:  string(update.LeaderAddr),
	})
}

func onStateUpdate(i *RpcInterface, state raft.RaftState) {
	switch state {
	case raft.Follower:
		break
	case raft.Leader:
		break
	case raft.Shutdown:
		i.Logger.Warn().Msg("Shutting down")
		break
	case raft.Candidate:
		i.Logger.Debug().Msg("Node is now Candidate")
		break
	}
	i.Logger.Trace().Msgf("State is now %s", state.String())
}

func ReplicatePeer(i *RpcInterface, update raft.PeerObservation) error {
	input := &fsmPb.WriteOperation{
		Operation: &fsmPb.WriteOperation_AddMember{AddMember: &fsmPb.AddMember{
			NodeId:  string(update.Peer.ID),
			Address: string(update.Peer.Address),
		}},
		Code: fsmPb.Operation_ADD_MEMBER,
	}
	val, _ := util.SerializeMessage(input)
	res := i.Raft.Apply(val, time.Second)
	if err := res.Error(); err != nil {
		log.Err(err)
		return err
	}
	err, isErr := res.Response().(error)
	if isErr {
		log.Err(err)
		return err
	}
	_, isValid := res.Response().(*fsmPb.AddMemberResult)
	if !isValid {
		log.Err(err)
		return err
	}
	return nil
}

func RemovePeer(i *RpcInterface, peerId string) error {
	input := &fsmPb.WriteOperation{
		Operation: &fsmPb.WriteOperation_RemoveMember{RemoveMember: &fsmPb.RemoveMember{
			NodeId: string(peerId),
		}},
		Code: fsmPb.Operation_REMOVE_MEMBER,
	}
	val, _ := util.SerializeMessage(input)
	res := i.Raft.Apply(val, time.Second)
	if err := res.Error(); err != nil {
		log.Err(err)
		return err
	}
	err, isErr := res.Response().(error)
	if isErr {
		log.Err(err)
		return err
	}
	_, isValid := res.Response().(*fsmPb.RemoveMemberResult)
	if !isValid {
		log.Err(err)
		return err
	}
	return nil
}

func (r RpcInterface) getCurShardInfo() *ShardInfo {
	return r.ClusterState.CurShardState.ShardInfo
}

func (r RpcInterface) getMemberMap() *haxmap.Map[string, MemberInfo] {
	return r.getCurShardInfo().MemberMap
}

func (r RpcInterface) getLeader() string {
	return r.getCurShardInfo().leader
}

func (r RpcInterface) getMemberInfo() *MemberInfo {
	return r.ClusterState.CurShardState.MemberInfo
}

func (r RpcInterface) getShardId() string {
	return r.getCurShardInfo().shardId
}

func (r RpcInterface) getShardState() *ShardState {
	return r.ClusterState.CurShardState
}

func (r RpcInterface) GetShardInfo(_ context.Context, req *pb.GetShardInfoRequest) (*pb.GetShardInfoResponse, error) {
	shardMap := r.getMemberMap()
	res := pb.GetShardInfoResponse{
		Info: &pb.ShardInfo{
			MemberAddressMap: map[string]*pb.MemberInfo{},
			LeaderId:         r.getLeader(),
			ShardId:          r.getShardId(),
		},
	}
	shardMap.ForEach(func(s string, info MemberInfo) bool {
		res.GetInfo().MemberAddressMap[s] = &pb.MemberInfo{
			NodeId:  info.NodeId,
			Address: info.Address,
		}
		return true
	})
	return &res, nil
}

func (r RpcInterface) GetClusterInfo(context.Context, *pb.GetClusterInfoRequest) (*pb.GetClusterInfoResponse, error) {
	clusterMap := r.ClusterState.ClusterInfo
	res := &pb.GetClusterInfoResponse{Info: &pb.ClusterInfo{}}
	resMap := make(map[string]*pb.ShardInfo)
	clusterMap.ForEach(func(s string, info *ShardInfo) bool {
		resMap[s] = &pb.ShardInfo{
			MemberAddressMap: toProtoMemberMap(info.MemberMap),
			LeaderId:         info.leader,
			ShardId:          info.shardId,
		}
		return true
	})
	res.Info.ShardMap = resMap
	return res, nil
}

func toProtoMemberMap(memberMap *haxmap.Map[string, MemberInfo]) map[string]*pb.MemberInfo {
	res := make(map[string]*pb.MemberInfo)
	memberMap.ForEach(func(s string, info MemberInfo) bool {
		res[s] = &pb.MemberInfo{
			NodeId:  info.NodeId,
			Address: info.Address,
		}
		return true
	})
	return res
}
