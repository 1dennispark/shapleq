package broker

import (
	"context"
	"fmt"
	"github.com/paust-team/paustq/broker/internals"
	"github.com/paust-team/paustq/broker/rpc"
	"github.com/paust-team/paustq/broker/storage"
	"github.com/paust-team/paustq/common"
	"github.com/paust-team/paustq/log"
	paustqproto "github.com/paust-team/paustq/proto"
	"github.com/paust-team/paustq/zookeeper"
	"google.golang.org/grpc"
	"net"
	"os"
	"time"
)

var (
	DefaultBrokerHomeDir = os.ExpandEnv("$HOME/.paustq")
	DefaultLogDir        = fmt.Sprintf("%s/log", DefaultBrokerHomeDir)
	DefaultDataDir       = fmt.Sprintf("%s/data", DefaultBrokerHomeDir)
	DefaultLogLevel      = logger.Info
)

type Broker struct {
	Port        uint16
	host        string
	grpcServer  *grpc.Server
	db          *storage.QRocksDB
	notifier    *internals.Notifier
	zkClient    *zookeeper.ZKClient
	broadcaster *internals.Broadcaster
	logDir      string
	dataDir     string
	logger      *logger.QLogger
}

func NewBroker(zkAddr string) *Broker {

	notifier := internals.NewNotifier()
	l := logger.NewQLogger("Broker", DefaultLogLevel)
	zkClient := zookeeper.NewZKClient(zkAddr).WithLogger(l)
	broadcaster := &internals.Broadcaster{}

	return &Broker{
		Port:        common.DefaultBrokerPort,
		notifier:    notifier,
		zkClient:    zkClient,
		broadcaster: broadcaster,
		logDir:      DefaultLogDir,
		dataDir:     DefaultDataDir,
		logger:      l,
	}
}

func (b *Broker) WithPort(port uint16) *Broker {
	b.Port = port
	return b
}

func (b *Broker) WithLogDir(dir string) *Broker {
	b.logDir = dir
	return b
}

func (b *Broker) WithDataDir(dir string) *Broker {
	b.dataDir = dir
	return b
}

func (b *Broker) WithLogLevel(level logger.LogLevel) *Broker {
	b.logger.SetLogLevel(level)
	return b
}

func (b *Broker) Start(ctx context.Context) error {
	defer b.Stop()

	errCh := make(chan error)
	defer close(errCh)

	// create directories for log and db
	if err := os.MkdirAll(b.dataDir, os.ModePerm); err != nil {
		b.logger.Error(err)
		return err
	}
	if err := os.MkdirAll(b.logDir, os.ModePerm); err != nil {
		b.logger.Error(err)
		return err
	}

	b.logger = b.logger.WithFile(DefaultLogDir)
	defer b.logger.Close()

	db, err := storage.NewQRocksDB(fmt.Sprintf("qstore-%d", time.Now().UnixNano()), b.dataDir)
	if err != nil {
		b.logger.Error(err)
		return err
	}
	b.db = db
	b.logger.Info("connected to rocksdb")

	// start grpc server
	b.grpcServer = grpc.NewServer()
	host, err := zookeeper.GetOutboundIP()
	if err != nil {
		b.logger.Error(err)
		return err
	}
	if !zookeeper.IsPublicIP(host) {
		b.logger.Warning("cannot attach to broker from external network")
	}

	b.host = host.String()
	b.zkClient = b.zkClient.WithLogger(b.logger)
	if err := b.zkClient.Connect(); err != nil {
		b.logger.Errorf("zk error: %v", err)
		return err
	}

	if err := b.zkClient.CreatePathsIfNotExist(); err != nil {
		b.logger.Errorf("zk error: %v", err)
		return err
	}

	if err := b.zkClient.AddBroker(b.host); err != nil {
		b.logger.Errorf("zk error: %v", err)
		return err
	}

	paustqproto.RegisterAPIServiceServer(b.grpcServer, rpc.NewAPIServiceServer(b.db, b.zkClient))
	paustqproto.RegisterStreamServiceServer(b.grpcServer,
		rpc.NewStreamServiceServer(b.db, b.notifier, b.zkClient, b.host, b.broadcaster, errCh))

	b.notifier.NotifyNews(ctx, errCh)

	startGrpcServer := func(server *grpc.Server, port uint16) {
		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			errCh <- err
			return
		}

		if err = server.Serve(lis); err != nil {
			errCh <- err
			return
		}
	}

	go startGrpcServer(b.grpcServer, b.Port)

	b.logger.Infof("start broker with port: %d", b.Port)

	select {
	case <-ctx.Done():
		b.logger.Info("received context done")
		return nil
	case err := <-errCh:
		b.logger.Error(err)
		return err
	}
}

func (b *Broker) Stop() {
	b.grpcServer.GracefulStop()
	b.db.Close()
	if err := b.zkClient.RemoveBroker(b.host); err != nil {
		b.logger.Error(err)
	}
	topics, _ := b.zkClient.GetTopics()
	for _, topic := range topics {
		if err := b.zkClient.RemoveTopicBroker(topic, b.host); err != nil {
			b.logger.Error(err)
		}
	}
	b.zkClient.Close()
	b.logger.Info("broker stopped")
}

func (b *Broker) Clean() {
	b.logger.Info("clean broker")
	_ = b.db.Destroy()
	os.RemoveAll(b.logDir)
	os.RemoveAll(b.dataDir)
}
