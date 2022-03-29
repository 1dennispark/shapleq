package broker

import (
	"context"
	"fmt"
	"github.com/paust-team/shapleq/broker/config"
	"github.com/paust-team/shapleq/broker/internals"
	"github.com/paust-team/shapleq/broker/service"
	"github.com/paust-team/shapleq/broker/storage"
	coordinator_helper "github.com/paust-team/shapleq/coordinator-helper"
	"github.com/paust-team/shapleq/log"
	"github.com/paust-team/shapleq/message"
	"github.com/paust-team/shapleq/pqerror"
	shapleq_proto "github.com/paust-team/shapleq/proto/pb"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"syscall"
)

type Broker struct {
	config          *config.BrokerConfig
	listener        net.Listener
	streamService   *service.StreamService
	sessionMgr      *internals.SessionManager
	txService       *service.TransactionService
	db              *storage.QRocksDB
	coordiWrapper   *coordinator_helper.CoordinatorWrapper
	logger          *logger.QLogger
	cancelBrokerCtx context.CancelFunc
	closed          bool
}

func NewBroker(config *config.BrokerConfig) *Broker {

	l := logger.NewQLogger("Broker", config.LogLevel())

	return &Broker{
		config: config,
		logger: l,
		closed: false,
	}
}

func (b *Broker) Config() *config.BrokerConfig {
	return b.config
}

func (b *Broker) Start() {
	brokerCtx, cancelFunc := context.WithCancel(context.Background())
	b.cancelBrokerCtx = cancelFunc

	if err := b.createDirs(); err != nil {
		b.logger.Fatal(err)
	}

	b.logger = b.logger.WithFile(b.config.LogDir())

	if err := b.connectToRocksDB(); err != nil {
		b.logger.Fatalf("error occurred while connecting to rocksdb : %v", err)
	}
	b.logger.Info("connected to rocksdb")

	if err := b.setUpZookeeper(); err != nil {
		b.logger.Fatalf("error occurred while setting up zookeeper : %v", err)
	}
	b.logger.Info("connected to zookeeper")

	tcpAddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("0.0.0.0:%d", b.config.Port()))
	if err != nil {
		b.logger.Fatalf("failed to resolve tcp address %s", err)
	}

	listenConfig := &net.ListenConfig{Control: reusePort}

	listener, err := listenConfig.Listen(brokerCtx, "tcp", tcpAddr.String())
	if err != nil {
		b.logger.Fatalf("fail to bind address to %d : %v", b.config.Port(), err)
	}
	b.listener = listener

	b.sessionMgr = internals.NewSessionManager()
	sessionAndContextCh, acceptErrCh := b.handleNewConnections(brokerCtx)

	txEventStreamCh, stEventStreamCh, sessionErrCh := b.generateEventStreams(sessionAndContextCh)

	b.streamService = service.NewStreamService(b.db, b.coordiWrapper, fmt.Sprintf("%s:%d", b.config.Hostname(), b.config.Port()))
	b.txService = service.NewTransactionService(b.db, b.coordiWrapper)

	sessionErrCh = pqerror.MergeErrors(sessionErrCh, b.streamService.HandleEventStreams(brokerCtx, stEventStreamCh))
	txErrCh := b.txService.HandleEventStreams(brokerCtx, txEventStreamCh)

	b.logger.Infof("start broker with port: %d", b.config.Port())

	for {
		select {
		case <-brokerCtx.Done():
			return
		case err := <-txErrCh:
			if err != nil {
				b.logger.Errorf("error occurred on transaction service: %s", err)
			}
		case <-acceptErrCh:
			return
		case sessionErr := <-sessionErrCh:
			if sessionErr != nil {
				sessErr, ok := sessionErr.(internals.SessionError)
				if !ok {
					b.logger.Errorf("unhandled error occurred : %v", sessionErr.Error())
					return
				}

				if _, ok := sessErr.PQError.(pqerror.SocketClosedError); ok {
					b.logger.Info("socket closed")
					sessErr.CancelSession()
					continue
				}

				b.logger.Errorf("error occurred from session : %v", sessErr)

				switch sessErr.PQError.(type) {
				case pqerror.IsClientVisible:
					sessErr.Session.Write(message.NewErrorAckMsg(sessErr.Code(), sessErr.Error()))
				case pqerror.IsBroadcastable:
					b.sessionMgr.BroadcastMsg(message.NewErrorAckMsg(sessErr.Code(), sessErr.Error()))
				default:
				}

				switch sessErr.PQError.(type) {
				case pqerror.IsBrokerStoppable:
					return
				case pqerror.IsSessionCloseable:
					sessErr.CancelSession()
				default:
				}
			}
		}
		runtime.Gosched()
	}
}

func (b *Broker) Stop() {
	b.closed = true
	b.cancelBrokerCtx()
	b.listener.Close()
	b.tearDownZookeeper()
	b.db.Close()
	b.logger.Info("broker stopped")
	b.logger.Close()
}

func (b *Broker) Clean() {
	b.logger.Info("clean broker")
	_ = b.db.Destroy()
	os.RemoveAll(b.config.LogDir())
	os.RemoveAll(b.config.DataDir())
}

func (b *Broker) createDirs() error {
	if err := os.MkdirAll(b.config.DataDir(), os.ModePerm); err != nil {
		return err
	}
	if err := os.MkdirAll(b.config.LogDir(), os.ModePerm); err != nil {
		return err
	}
	return nil
}

func (b *Broker) connectToRocksDB() error {
	db, err := storage.NewQRocksDB("shapleq-store", b.config.DataDir())
	if err != nil {
		return err
	}
	b.db = db
	return nil
}

func (b *Broker) setUpZookeeper() error {

	b.coordiWrapper = coordinator_helper.NewCoordinatorWrapper(b.config.ZKQuorum(), b.config.ZKTimeout(), b.config.ZKFlushInterval(), b.logger)
	if err := b.coordiWrapper.Connect(); err != nil {
		return err
	}

	if err := b.coordiWrapper.CreatePathsIfNotExist(); err != nil {
		return err
	}

	if err := b.coordiWrapper.AddBroker(b.config.Hostname() + ":" + strconv.Itoa(int(b.config.Port()))); err != nil {
		return err
	}

	return nil
}

func (b *Broker) tearDownZookeeper() {
	_ = b.coordiWrapper.RemoveBroker(b.config.Hostname())
	topics, _ := b.coordiWrapper.GetTopicFrames()
	for _, topic := range topics {
		fragments, _ := b.coordiWrapper.GetTopicFragments(topic)
		for _, fragment := range fragments {
			fragmentId, err := strconv.ParseUint(fragment, 10, 32)
			if err != nil {
				b.logger.Errorf("error occurred on tearing down zookeeper: %s", err.Error())
				continue
			}
			_ = b.coordiWrapper.RemoveBrokerOfTopic(topic, uint32(fragmentId), b.config.Hostname())
		}
	}
	b.coordiWrapper.Close()
}

type SessionAndContext struct {
	session       *internals.Session
	ctx           context.Context
	cancelSession context.CancelFunc
}

func (b *Broker) handleNewConnections(brokerCtx context.Context) (<-chan SessionAndContext, <-chan error) {
	sessionCtxCh := make(chan SessionAndContext)
	errCh := make(chan error)

	go func() {
		defer close(sessionCtxCh)
		defer close(errCh)
		for {
			conn, err := b.listener.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok {
					if ne.Temporary() {
						b.logger.Infof("temporary error occurred while accepting new connections : %v", err)
						continue
					}
				}
				if !b.closed {
					b.logger.Errorf("error occurred while accepting new connections : %v", err)
					errCh <- err
				}

				return
			}

			sessionCtx, cancelSession := context.WithCancel(brokerCtx)

			select {
			case sessionCtxCh <- SessionAndContext{internals.NewSession(conn, b.config.Timeout()), sessionCtx, cancelSession}:

				b.logger.Info("new connection created")
			case <-brokerCtx.Done():
				return
			}
		}
	}()

	return sessionCtxCh, errCh
}

func (b *Broker) generateEventStreams(scCh <-chan SessionAndContext) (<-chan internals.EventStream, <-chan internals.EventStream, <-chan error) {
	transactionalEvents := make(chan internals.EventStream)
	streamingEvents := make(chan internals.EventStream)
	sessionErrCh := make(chan error)
	wg := sync.WaitGroup{}

	go func() {
		defer close(transactionalEvents)
		defer close(streamingEvents)
		defer close(sessionErrCh)
		defer wg.Wait()

		for sessAndCtx := range scCh {
			txMsgCh := make(chan *message.QMessage)
			streamMsgCh := make(chan *message.QMessage)

			transactionalEvents <- internals.EventStream{sessAndCtx.session, txMsgCh, sessAndCtx.ctx, sessAndCtx.cancelSession}
			streamingEvents <- internals.EventStream{sessAndCtx.session, streamMsgCh, sessAndCtx.ctx, sessAndCtx.cancelSession}

			wg.Add(1)
			go func(sc SessionAndContext) {
				defer close(txMsgCh)
				defer close(streamMsgCh)
				defer wg.Done()

				b.sessionMgr.AddSession(sc.session)
				defer b.sessionMgr.RemoveSession(sc.session)

				sc.session.Open()
				defer func() {
					sc.session.Close()
					switch sc.session.Type() {
					case shapleq_proto.SessionType_PUBLISHER:
						for _, topic := range sc.session.Topics() {
							_, _ = b.coordiWrapper.AddNumPublishers(topic.TopicName(), -1)
						}
					case shapleq_proto.SessionType_SUBSCRIBER:
						for _, topic := range sc.session.Topics() {
							for _, id := range topic.FragmentIds() {
								_, _ = b.coordiWrapper.AddNumSubscriber(topic.TopicName(), id, -1)
							}
						}
					}
				}()

				msgCh, errCh, err := sc.session.ContinuousRead(sc.ctx)
				if err != nil {
					return
				}

				for {
					select {
					case msg, ok := <-msgCh:
						if !ok {
							return
						}
						if msg != nil {
							if msg.Type() == message.TRANSACTION {
								txMsgCh <- msg
							} else if msg.Type() == message.STREAM {
								streamMsgCh <- msg
							}
						}

					case err, ok := <-errCh:
						if !ok {
							return
						}
						if err != nil {
							pqErr, ok := err.(pqerror.PQError)
							if !ok {
								sessionErrCh <- internals.SessionError{
									PQError:       pqerror.UnhandledError{ErrStr: err.Error()},
									Session:       sc.session,
									CancelSession: sc.cancelSession}
							} else {
								sessionErrCh <- internals.SessionError{
									PQError:       pqErr,
									Session:       sc.session,
									CancelSession: sc.cancelSession}
							}
						}
					}
				}
			}(sessAndCtx)
		}
	}()

	return transactionalEvents, streamingEvents, sessionErrCh
}

func reusePort(network, address string, conn syscall.RawConn) error {
	return conn.Control(func(descriptor uintptr) {
		if err := unix.SetsockoptInt(int(descriptor), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			panic(err)
		}
	})
}
