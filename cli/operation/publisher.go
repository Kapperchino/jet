package operation

import (
	"fmt"
	"github.com/Kapperchino/jet-stream/application/proto/proto"
	"github.com/Kapperchino/jet-stream/client"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v2"
)

const CliDir = "/.config/jet-cli/"
const CliFile = "config.json"

type Publisher struct {
	client *client.JetClient
}

func (p Publisher) process(cCtx *cli.Context) (any, error) {
	messageStr := cCtx.StringSlice("messages")
	topic := cCtx.String("topic")
	arr := make([]*proto.KeyVal, 0)
	for i := 0; i < len(messageStr)-1; i += 2 {
		arr = append(arr, &proto.KeyVal{
			Key: []byte(messageStr[i]),
			Val: []byte(messageStr[i+1]),
		})
	}
	res, err := p.client.PublishMessage(arr, topic)
	if err != nil {
		log.Err(err).Msgf("Error when publishing")
		return nil, err
	}
	return res, nil
}

func (p Publisher) Serialize(cCtx *cli.Context) error {
	res, _ := p.process(cCtx)
	fmt.Printf("%v\n", res)
	return nil
}
