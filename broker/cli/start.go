package cli

import (
	"context"
	"github.com/paust-team/paustq/broker"
	"github.com/paust-team/paustq/common"
	"github.com/spf13/cobra"
	"log"
)

var (
	logDir    	string
	dataDir    	string
	port   		uint16
	zkAddr 		string
)

func NewStartCmd() *cobra.Command {

	var startCmd = &cobra.Command{
		Use:   "start",
		Short: "start paustq broker",
		Run: func(cmd *cobra.Command, args []string) {
			brokerInstance, err := broker.NewBroker(zkAddr)
			if err != nil {
				log.Fatal(err)
			}

			if port != common.DefaultBrokerPort {
				brokerInstance = brokerInstance.WithPort(port)
			}
			if err := brokerInstance.Start(context.Background()); err != nil {
				log.Fatal(err)
			}
		},
	}

	startCmd.Flags().StringVar(&logDir, "log-dir", broker.DefaultLogDir, "log directory")
	startCmd.Flags().StringVar(&dataDir, "data-dir", broker.DefaultDataDir, "data directory")
	startCmd.Flags().Uint16Var(&port, "port", common.DefaultBrokerPort, "broker port")
	startCmd.Flags().StringVarP(&zkAddr, "zk-addr", "z", "127.0.0.1", "zookeeper ip address")

	startCmd.MarkFlagRequired("zk-addr")

	return startCmd
}
