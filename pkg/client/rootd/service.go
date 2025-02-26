package rootd

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/datawire/dlib/derror"
	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dhttp"
	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/rpc/v2/common"
	rpc "github.com/telepresenceio/telepresence/rpc/v2/daemon"
	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/client/logging"
	"github.com/telepresenceio/telepresence/v2/pkg/client/scout"
	"github.com/telepresenceio/telepresence/v2/pkg/client/socket"
	"github.com/telepresenceio/telepresence/v2/pkg/filelocation"
	"github.com/telepresenceio/telepresence/v2/pkg/log"
	"github.com/telepresenceio/telepresence/v2/pkg/pprof"
	"github.com/telepresenceio/telepresence/v2/pkg/proc"
	"github.com/telepresenceio/telepresence/v2/pkg/tracing"
	"github.com/telepresenceio/telepresence/v2/pkg/vif"
)

type NewServiceFunc func(*scout.Reporter, *client.Config) *Service

type newServiceKey struct{}

func WithNewServiceFunc(ctx context.Context, f NewServiceFunc) context.Context {
	return context.WithValue(ctx, newServiceKey{}, f)
}

func GetNewServiceFunc(ctx context.Context) NewServiceFunc {
	if f, ok := ctx.Value(newServiceKey{}).(NewServiceFunc); ok {
		return f
	}
	panic("No User daemon Service creator has been registered")
}

const (
	ProcessName = "daemon"
	titleName   = "Daemon"
	pprofFlag   = "pprof"
)

func help() string {
	return `The Telepresence ` + titleName + ` is a long-lived background component that manages
connections and network state.

Launch the Telepresence ` + titleName + `:
    sudo telepresence Service

Examine the ` + titleName + `'s log output in
    ` + filepath.Join(filelocation.AppUserLogDir(context.Background()), ProcessName+".log") + `
to troubleshoot problems.
`
}

type sessionReply struct {
	status *rpc.DaemonStatus
	err    error
}

// Service represents the state of the Telepresence Daemon.
type Service struct {
	rpc.UnsafeDaemonServer
	quit            context.CancelFunc
	connectCh       chan *rpc.OutboundInfo
	connectReplyCh  chan sessionReply
	sessionLock     sync.RWMutex
	sessionCancel   context.CancelFunc
	sessionContext  context.Context
	sessionQuitting int32 // atomic boolean. True if non-zero.
	session         *Session
	timedLogLevel   log.TimedLevel

	scout *scout.Reporter
}

func NewService(sr *scout.Reporter, cfg *client.Config) *Service {
	return &Service{
		scout:          sr,
		timedLogLevel:  log.NewTimedLevel(cfg.LogLevels.RootDaemon.String(), log.SetLevel),
		connectCh:      make(chan *rpc.OutboundInfo),
		connectReplyCh: make(chan sessionReply),
	}
}

func (s *Service) As(ptr any) {
	if sp, ok := ptr.(**Service); ok {
		*sp = s
	} else {
		panic(fmt.Sprintf("%T does not implement %T", s, *sp))
	}
}

// Command returns the telepresence sub-command "daemon-foreground".
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:    ProcessName + "-foreground <logging dir> <config dir>",
		Short:  "Launch Telepresence " + titleName + " in the foreground (debug)",
		Args:   cobra.ExactArgs(2),
		Hidden: true,
		Long:   help(),
		RunE:   run,
	}
	flags := cmd.Flags()
	flags.Uint16(pprofFlag, 0, "start pprof server on the given port")
	return cmd
}

func (s *Service) Version(_ context.Context, _ *empty.Empty) (*common.VersionInfo, error) {
	return &common.VersionInfo{
		ApiVersion: client.APIVersion,
		Version:    client.Version(),
		Name:       client.DisplayName,
	}, nil
}

func (s *Service) Status(_ context.Context, _ *empty.Empty) (*rpc.DaemonStatus, error) {
	s.sessionLock.RLock()
	defer s.sessionLock.RUnlock()
	r := &rpc.DaemonStatus{
		Version: &common.VersionInfo{
			ApiVersion: client.APIVersion,
			Version:    client.Version(),
			Name:       client.DisplayName,
		},
	}
	if s.session != nil {
		r.OutboundConfig = s.session.getNetworkConfig().OutboundInfo
	}
	return r, nil
}

func (s *Service) Quit(ctx context.Context, _ *empty.Empty) (*empty.Empty, error) {
	dlog.Debug(ctx, "Received gRPC Quit")
	if !s.sessionLock.TryRLock() {
		// A running session is blocking with a write-lock. Give it some time to quit, then kill it
		time.Sleep(2 * time.Second)
		if !s.sessionLock.TryRLock() {
			s.quit()
			return &empty.Empty{}, nil
		}
	}
	defer s.sessionLock.RUnlock()
	s.cancelSessionReadLocked()
	s.quit()
	return &empty.Empty{}, nil
}

func (s *Service) SetDnsSearchPath(ctx context.Context, paths *rpc.Paths) (*empty.Empty, error) {
	err := s.WithSession(func(ctx context.Context, session *Session) error {
		session.SetSearchPath(ctx, paths.Paths, paths.Namespaces)
		return nil
	})
	return &empty.Empty{}, err
}

func (s *Service) SetDNSExcludes(ctx context.Context, req *rpc.SetDNSExcludesRequest) (*emptypb.Empty, error) {
	err := s.WithSession(func(c context.Context, session *Session) error {
		session.SetExcludes(c, req.Excludes)
		return nil
	})
	return &emptypb.Empty{}, err
}

func (s *Service) SetDNSMappings(ctx context.Context, req *rpc.SetDNSMappingsRequest) (*emptypb.Empty, error) {
	err := s.WithSession(func(c context.Context, session *Session) error {
		session.SetMappings(c, req.Mappings)
		return nil
	})
	return &emptypb.Empty{}, err
}

func (s *Service) Connect(ctx context.Context, info *rpc.OutboundInfo) (*rpc.DaemonStatus, error) {
	dlog.Debug(ctx, "Received gRPC Connect")
	select {
	case <-ctx.Done():
		return nil, status.Error(codes.Canceled, ctx.Err().Error())
	case s.connectCh <- info:
	}
	select {
	case <-ctx.Done():
		return nil, status.Error(codes.Canceled, ctx.Err().Error())
	case reply := <-s.connectReplyCh:
		return reply.status, reply.err
	}
}

func (s *Service) Disconnect(ctx context.Context, _ *empty.Empty) (*empty.Empty, error) {
	dlog.Debug(ctx, "Received gRPC Disconnect")
	s.cancelSession()
	return &empty.Empty{}, nil
}

func (s *Service) WaitForNetwork(ctx context.Context, e *empty.Empty) (*empty.Empty, error) {
	err := s.WithSession(func(ctx context.Context, session *Session) error {
		if err, ok := <-session.networkReady(ctx); ok {
			return status.Error(codes.Unavailable, err.Error())
		}
		return nil
	})
	return &empty.Empty{}, err
}

func (s *Service) cancelSessionReadLocked() {
	if s.sessionCancel != nil {
		s.sessionCancel()
	}
}

func (s *Service) cancelSession() {
	if !atomic.CompareAndSwapInt32(&s.sessionQuitting, 0, 1) {
		return
	}
	s.sessionLock.RLock()
	s.cancelSessionReadLocked()
	s.sessionLock.RUnlock()

	s.sessionLock.Lock()
	s.session = nil
	s.sessionCancel = nil
	atomic.StoreInt32(&s.sessionQuitting, 0)
	s.sessionLock.Unlock()
}

func (s *Service) WithSession(f func(context.Context, *Session) error) error {
	if atomic.LoadInt32(&s.sessionQuitting) != 0 {
		return status.Error(codes.Canceled, "session cancelled")
	}
	s.sessionLock.RLock()
	defer s.sessionLock.RUnlock()
	if s.session == nil {
		return status.Error(codes.Unavailable, "no active session")
	}
	return f(s.sessionContext, s.session)
}

func (s *Service) GetNetworkConfig(ctx context.Context, e *empty.Empty) (nc *rpc.NetworkConfig, err error) {
	err = s.WithSession(func(ctx context.Context, session *Session) error {
		nc = session.getNetworkConfig()
		return nil
	})
	return
}

func (s *Service) SetLogLevel(ctx context.Context, request *manager.LogLevelRequest) (*empty.Empty, error) {
	duration := time.Duration(0)
	if request.Duration != nil {
		duration = request.Duration.AsDuration()
	}
	return &empty.Empty{}, logging.SetAndStoreTimedLevel(ctx, s.timedLogLevel, request.LogLevel, duration, ProcessName)
}

func (s *Service) configReload(c context.Context) error {
	return client.Watch(c, func(c context.Context) error {
		s.sessionLock.RLock()
		defer s.sessionLock.RUnlock()
		if s.session == nil {
			return client.RestoreDefaults(c, true)
		}
		return s.session.applyConfig(c)
	})
}

// manageSessions is the counterpart to the Connect method. It reads the connectCh, creates
// a session and writes a reply to the connectErrCh. The session is then started if it was
// successfully created.
func (s *Service) manageSessions(c context.Context) error {
	// The d.quit is called when we receive a Quit. Since it
	// terminates this function, it terminates the whole process.
	wg := sync.WaitGroup{}
	defer wg.Wait()
	c, s.quit = context.WithCancel(c)

	for {
		// Wait for a connection request
		select {
		case <-c.Done():
			return nil
		case oi := <-s.connectCh:
			reply := s.startSession(c, oi, &wg)
			select {
			case <-c.Done():
				return nil
			case s.connectReplyCh <- reply:
			default:
				// Nobody left to read the response? That's fine really. Just means that
				// whoever wanted to start the session terminated early.
				s.cancelSession()
			}
		}
	}
}

func (s *Service) startSession(ctx context.Context, oi *rpc.OutboundInfo, wg *sync.WaitGroup) sessionReply {
	s.sessionLock.Lock() // Locked during creation
	defer s.sessionLock.Unlock()
	reply := sessionReply{
		status: &rpc.DaemonStatus{
			Version: &common.VersionInfo{
				ApiVersion: client.APIVersion,
				Version:    client.Version(),
			},
		},
	}
	if s.session != nil {
		reply.status.OutboundConfig = s.session.getNetworkConfig().OutboundInfo
		return reply
	}

	ctx, cancel := context.WithCancel(ctx)
	session, err := GetNewSessionFunc(ctx)(ctx, s.scout, oi)
	if ctx.Err() != nil || err != nil {
		cancel()
		reply.err = err
		return reply
	}

	s.session = session
	s.sessionContext = ctx
	s.sessionCancel = func() {
		cancel()
		<-session.Done()
	}
	if err := s.session.applyConfig(ctx); err != nil {
		dlog.Warnf(ctx, "failed to apply config from traffic-manager: %v", err)
	}

	reply.status.OutboundConfig = s.session.getNetworkConfig().OutboundInfo

	initErrCh := make(chan error, 1)

	// Run the session asynchronously. We must be able to respond to connect (with getNetworkConfig) while
	// the session is running. The d.session.cancel is called from Disconnect
	wg.Add(1)
	go func() {
		defer func() {
			s.sessionLock.Lock()
			s.session = nil
			s.sessionCancel = nil
			if err := client.RestoreDefaults(ctx, true); err != nil {
				dlog.Warn(ctx, err)
			}
			s.sessionLock.Unlock()
			wg.Done()
		}()
		if err := s.session.run(s.sessionContext, initErrCh); err != nil {
			dlog.Error(ctx, err)
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-initErrCh:
		if err != nil {
			reply.err = err
			s.cancelSessionReadLocked()
		}
	}
	return reply
}

func (s *Service) serveGrpc(c context.Context, l net.Listener, tracer common.TracingServer) error {
	defer func() {
		// Error recovery.
		if perr := derror.PanicToError(recover()); perr != nil {
			dlog.Errorf(c, "%+v", perr)
		}
	}()

	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(otelgrpc.UnaryServerInterceptor()),
		grpc.StreamInterceptor(otelgrpc.StreamServerInterceptor()),
	}
	cfg := client.GetConfig(c)
	if !cfg.Grpc.MaxReceiveSize.IsZero() {
		if mz, ok := cfg.Grpc.MaxReceiveSize.AsInt64(); ok {
			opts = append(opts, grpc.MaxRecvMsgSize(int(mz)))
		}
	}
	svc := grpc.NewServer(opts...)
	rpc.RegisterDaemonServer(svc, s)
	common.RegisterTracingServer(svc, tracer)

	sc := &dhttp.ServerConfig{
		Handler: svc,
	}
	dlog.Info(c, "gRPC server started")
	err := sc.Serve(c, l)
	if err != nil {
		dlog.Errorf(c, "gRPC server ended with: %v", err)
	} else {
		dlog.Debug(c, "gRPC server ended")
	}
	return err
}

// run is the main function when executing as the daemon.
func run(cmd *cobra.Command, args []string) error {
	if !proc.IsAdmin() {
		return fmt.Errorf("telepresence %s must run with elevated privileges", ProcessName)
	}

	// seed random generator (used when shuffling IPs)
	rand.Seed(time.Now().UnixNano())

	loggingDir := args[0]
	configDir := args[1]
	c := cmd.Context()

	// Spoof the AppUserLogDir and AppUserConfigDir so that they return the original user's
	// directories rather than directories for the root user.
	c = filelocation.WithAppUserLogDir(c, loggingDir)
	c = filelocation.WithAppUserConfigDir(c, configDir)

	cfg, err := client.LoadConfig(c)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	c = client.WithConfig(c, cfg)
	flags := cmd.Flags()
	if pprofPort, _ := flags.GetUint16(pprofFlag); pprofPort > 0 {
		go func() {
			if err := pprof.PprofServer(c, pprofPort); err != nil {
				dlog.Error(c, err)
			}
		}()
	}

	c = dgroup.WithGoroutineName(c, "/"+ProcessName)
	c, err = logging.InitContext(c, ProcessName, logging.RotateDaily, true)
	if err != nil {
		return err
	}

	tracer, err := tracing.NewTraceServer(c, "root-daemon")
	if err != nil {
		return err
	}

	dlog.Info(c, "---")
	dlog.Infof(c, "Telepresence %s %s starting...", ProcessName, client.DisplayVersion())
	dlog.Infof(c, "PID is %d", os.Getpid())
	dlog.Info(c, "")

	// Listen on domain unix domain socket or windows named pipe. The listener must be opened
	// before other tasks because the CLI client will only wait for a short period of time for
	// the socket/pipe to appear before it gives up.
	grpcListener, err := socket.Listen(c, ProcessName, socket.RootDaemonPath(c))
	if err != nil {
		return err
	}
	defer func() {
		_ = socket.Remove(grpcListener)
	}()
	dlog.Debug(c, "Listener opened")

	d := GetNewServiceFunc(c)(scout.NewReporter(c, "daemon"), cfg)
	if err = logging.LoadTimedLevelFromCache(c, d.timedLogLevel, ProcessName); err != nil {
		return err
	}
	vif.InitLogger(c)

	g := dgroup.NewGroup(c, dgroup.GroupConfig{
		SoftShutdownTimeout:  2 * time.Second,
		EnableSignalHandling: true,
		ShutdownOnNonError:   true,
	})

	// Add a reload function that triggers on create and write of the config.yml file.
	g.Go("config-reload", d.configReload)
	g.Go("session", d.manageSessions)
	g.Go("server-grpc", func(c context.Context) error { return d.serveGrpc(c, grpcListener, tracer) })
	g.Go("metriton", d.scout.Run)
	err = g.Wait()
	if err != nil {
		dlog.Error(c, err)
	}
	return err
}
