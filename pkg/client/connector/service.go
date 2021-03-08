package connector

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/datawire/dlib/derror"
	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dlog"
	"github.com/datawire/telepresence2/rpc/v2/common"
	rpc "github.com/datawire/telepresence2/rpc/v2/connector"
	"github.com/datawire/telepresence2/rpc/v2/daemon"
	"github.com/datawire/telepresence2/rpc/v2/manager"
	"github.com/datawire/telepresence2/v2/pkg/client"
	"github.com/datawire/telepresence2/v2/pkg/client/logging"
	"github.com/datawire/telepresence2/v2/pkg/filelocation"
)

const processName = "connector"
const titleName = "Connector"

var help = `The Telepresence ` + titleName + ` is a background component that manages a connection. It
requires that a daemon is already running.

Launch the Telepresence ` + titleName + `:
    telepresence connect

Examine the ` + titleName + `'s log output in
    ` + filepath.Join(func() string { dir, _ := filelocation.AppUserLogDir(context.Background()); return dir }(), processName+".log") + `
to troubleshoot problems.
`

// service represents the state of the Telepresence Connector
type service struct {
	rpc.UnsafeConnectorServer

	env    client.Env
	daemon daemon.DaemonClient
	scout  *client.Scout

	managerProxy mgrProxy
	ctx          context.Context
	cancel       func()

	connectMu sync.Mutex
	// These get set by .connect() and are protected by connectMu.
	cluster    *k8sCluster
	bridge     *bridge
	trafficMgr *trafficManager
}

// Command returns the CLI sub-command for "connector-foreground"
func Command() *cobra.Command {
	c := &cobra.Command{
		Use:    processName + "-foreground",
		Short:  "Launch Telepresence " + titleName + " in the foreground (debug)",
		Args:   cobra.ExactArgs(0),
		Hidden: true,
		Long:   help,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context())
		},
	}
	return c
}

type callCtx struct {
	context.Context
	caller context.Context
}

func (c callCtx) Deadline() (deadline time.Time, ok bool) {
	if dl, ok := c.Context.Deadline(); ok {
		return dl, true
	}
	return c.caller.Deadline()
}

func (c callCtx) Done() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		select {
		case <-c.Context.Done():
			close(ch)
		case <-c.caller.Done():
			close(ch)
		}
	}()
	return ch
}

func (c callCtx) Err() error {
	err := c.Context.Err()
	if err == nil {
		err = c.caller.Err()
	}
	return err
}

func (c callCtx) Value(key interface{}) interface{} {
	return c.Context.Value(key)
}

func callRecovery(c context.Context, r interface{}, err error) error {
	perr := derror.PanicToError(r)
	if perr != nil {
		if err == nil {
			err = perr
		} else {
			dlog.Errorf(c, "%+v", perr)
		}
	}
	if err != nil {
		dlog.Errorf(c, "%+v", err)
	}
	return err
}

var ucn int64 = 0

func nextUcn() int {
	return int(atomic.AddInt64(&ucn, 1))
}

func callName(s string) string {
	return fmt.Sprintf("%s-%d", s, nextUcn())
}

func (s *service) callCtx(c context.Context, name string) context.Context {
	return dgroup.WithGoroutineName(&callCtx{Context: s.ctx, caller: c}, callName(name))
}

func (s *service) Version(_ context.Context, _ *empty.Empty) (*common.VersionInfo, error) {
	return &common.VersionInfo{
		ApiVersion: client.APIVersion,
		Version:    client.Version(),
	}, nil
}

func (s *service) Connect(c context.Context, cr *rpc.ConnectRequest) (ci *rpc.ConnectInfo, err error) {
	c = s.callCtx(c, "Connect")
	defer func() { err = callRecovery(c, recover(), err) }()
	return s.connect(c, cr), nil
}

func (s *service) CreateIntercept(c context.Context, ir *rpc.CreateInterceptRequest) (result *rpc.InterceptResult, err error) {
	ie, is := s.interceptStatus()
	if ie != rpc.InterceptError_UNSPECIFIED {
		return &rpc.InterceptResult{Error: ie, ErrorText: is}, nil
	}
	c = s.callCtx(c, "CreateIntercept")
	defer func() { err = callRecovery(c, recover(), err) }()
	return s.trafficMgr.addIntercept(c, ir)
}

func (s *service) RemoveIntercept(c context.Context, rr *manager.RemoveInterceptRequest2) (result *rpc.InterceptResult, err error) {
	ie, is := s.interceptStatus()
	if ie != rpc.InterceptError_UNSPECIFIED {
		return &rpc.InterceptResult{Error: ie, ErrorText: is}, nil
	}
	c = s.callCtx(c, "RemoveIntercept")
	defer func() { err = callRecovery(c, recover(), err) }()
	err = s.trafficMgr.removeIntercept(c, rr.Name)
	return &rpc.InterceptResult{}, err
}

func (s *service) List(ctx context.Context, lr *rpc.ListRequest) (*rpc.DeploymentInfoSnapshot, error) {
	if s.trafficMgr.managerClient == nil {
		return &rpc.DeploymentInfoSnapshot{}, nil
	}
	return s.trafficMgr.deploymentInfoSnapshot(ctx, lr), nil
}

func (s *service) Uninstall(c context.Context, ur *rpc.UninstallRequest) (result *rpc.UninstallResult, err error) {
	c = s.callCtx(c, "Uninstall")
	defer func() { err = callRecovery(c, recover(), err) }()
	return s.trafficMgr.uninstall(c, ur)
}

func (s *service) Quit(_ context.Context, _ *empty.Empty) (*empty.Empty, error) {
	s.cancel()
	return &empty.Empty{}, nil
}

// connect the connector to a cluster
func (s *service) connect(c context.Context, cr *rpc.ConnectRequest) *rpc.ConnectInfo {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	k8sConfig, err := newK8sConfig(cr.KubeFlags)
	if err != nil {
		return &rpc.ConnectInfo{
			Error:     rpc.ConnectInfo_CLUSTER_FAILED,
			ErrorText: err.Error(),
		}
	}
	// Check for the various "this is not the first call to Connect" states:
	switch {
	case s.cluster != nil && s.cluster.k8sConfig.equals(k8sConfig):
		if mns := cr.MappedNamespaces; len(mns) > 0 {
			if len(mns) == 1 && mns[0] == "all" {
				mns = nil
			}
			sort.Strings(mns)
			s.cluster.setMappedNamespaces(c, mns)
		}
		ret := &rpc.ConnectInfo{
			Error:          rpc.ConnectInfo_ALREADY_CONNECTED,
			ClusterOk:      true,
			ClusterContext: s.cluster.k8sConfig.Context,
			ClusterServer:  s.cluster.k8sConfig.Server,
			ClusterId:      s.cluster.getClusterId(c),
			BridgeOk:       s.bridge.check(c),
			IngressInfos:   s.cluster.detectIngressBehavior(),
		}
		s.trafficMgr.setStatus(c, ret)
		return ret
	case s.cluster != nil /* && !s.cluster.k8sConfig.equals(k8sConfig) */ :
		ret := &rpc.ConnectInfo{
			Error:          rpc.ConnectInfo_MUST_RESTART,
			ClusterOk:      true,
			ClusterContext: s.cluster.k8sConfig.Context,
			ClusterServer:  s.cluster.k8sConfig.Server,
			ClusterId:      s.cluster.getClusterId(c),
			BridgeOk:       s.bridge.check(c),
			IngressInfos:   s.cluster.detectIngressBehavior(),
		}
		s.trafficMgr.setStatus(c, ret)
		return ret
	case /* s.cluster == nil && */ s.bridge != nil:
		// How can this ever happen?  What sets s.cluster = nil when Quit is initiated?
		return &rpc.ConnectInfo{
			Error: rpc.ConnectInfo_DISCONNECTING,
		}
	default:
		// Proceed; this is the first call to Connect.
	}

	mappedNamespaces := cr.MappedNamespaces
	if len(mappedNamespaces) == 1 && mappedNamespaces[0] == "all" {
		mappedNamespaces = nil
	}
	sort.Strings(mappedNamespaces)

	_ = s.scout.Report(s.ctx, "connect")

	dlog.Info(c, "Connecting to traffic manager...")
	cluster, err := trackKCluster(s.ctx, k8sConfig, mappedNamespaces, s.daemon)
	if err != nil {
		dlog.Errorf(c, "unable to track k8s cluster: %+v", err)
		s.cancel()
		return &rpc.ConnectInfo{
			Error:     rpc.ConnectInfo_CLUSTER_FAILED,
			ErrorText: err.Error(),
		}
	}
	s.cluster = cluster
	dlog.Infof(c, "Connected to context %s (%s)", s.cluster.Context, s.cluster.Server)

	k8sObjectMap := cluster.findNumK8sObjects()
	// Phone home with the information about the size of the cluster
	s.scout.SetMetadatum("cluster_id", s.cluster.getClusterId(c))
	for objectType, num := range k8sObjectMap {
		s.scout.SetMetadatum(objectType, num)
	}
	s.scout.SetMetadatum("mapped_namespaces", len(cr.MappedNamespaces))
	_ = s.scout.Report(s.ctx, "connecting_traffic_manager")

	connectStart := time.Now()
	tmgr, err := newTrafficManager(s.ctx, s.env, s.cluster, s.scout.Reporter.InstallID())
	if err != nil {
		dlog.Errorf(c, "Unable to connect to TrafficManager: %s", err)
		// No point in continuing without a traffic manager
		s.cancel()
		return &rpc.ConnectInfo{
			Error:     rpc.ConnectInfo_TRAFFIC_MANAGER_FAILED,
			ErrorText: err.Error(),
		}
	}

	s.trafficMgr = tmgr
	// Wait for traffic manager to connect
	dlog.Info(c, "Waiting for TrafficManager to connect")
	if err := tmgr.waitUntilStarted(c); err != nil {
		dlog.Errorf(c, "Failed to start traffic-manager: %v", err)
		// No point in continuing without a traffic manager
		s.cancel()
		return &rpc.ConnectInfo{
			Error:     rpc.ConnectInfo_TRAFFIC_MANAGER_FAILED,
			ErrorText: err.Error(),
		}
	}
	s.managerProxy.SetClient(tmgr.managerClient)

	dlog.Infof(c, "Starting traffic-manager bridge in context %s", cluster.Context)
	br := newBridge(s.daemon, tmgr.sshPort)
	err = br.start(s.ctx)
	if err != nil {
		dlog.Errorf(c, "Failed to start traffic-manager bridge: %v", err)
		// No point in continuing without a bridge
		s.cancel()
		return &rpc.ConnectInfo{
			Error:     rpc.ConnectInfo_BRIDGE_FAILED,
			ErrorText: err.Error(),
		}
	}

	s.bridge = br

	// Collect data on how long connection time took
	_ = s.scout.Report(s.ctx, "finished_connecting_traffic_manager",
		client.ScoutMeta{Key: "connect_duration", Value: time.Since(connectStart).Seconds()})

	ret := &rpc.ConnectInfo{
		Error:          rpc.ConnectInfo_UNSPECIFIED,
		ClusterOk:      true,
		ClusterContext: s.cluster.k8sConfig.Context,
		ClusterServer:  s.cluster.k8sConfig.Server,
		ClusterId:      s.cluster.getClusterId(c),
		BridgeOk:       s.bridge.check(c),
		IngressInfos:   s.cluster.detectIngressBehavior(),
	}
	s.trafficMgr.setStatus(c, ret)
	return ret
}

// run is the main function when executing as the connector
func run(c context.Context) error {
	c, err := logging.InitContext(c, processName)
	if err != nil {
		return err
	}
	c = dgroup.WithGoroutineName(c, "/"+processName)

	env, err := client.LoadEnv(c)
	if err != nil {
		return err
	}

	// establish a connection to the daemon gRPC service
	conn, err := client.DialSocket(c, client.DaemonSocketName)
	if err != nil {
		return err
	}
	defer conn.Close()
	s := &service{
		env:    env,
		daemon: daemon.NewDaemonClient(conn),
		scout:  client.NewScout(c, "connector"),
	}

	g := dgroup.NewGroup(c, dgroup.GroupConfig{
		SoftShutdownTimeout:  2 * time.Second,
		EnableSignalHandling: true,
		ShutdownOnNonError:   true,
	})
	s.cancel = func() { g.Go("quit", func(_ context.Context) error { return nil }) }

	dlog.Info(c, "---")
	dlog.Infof(c, "Telepresence %s %s starting...", titleName, client.DisplayVersion())
	dlog.Infof(c, "PID is %d", os.Getpid())
	dlog.Info(c, "")

	g.Go("server-grpc", func(c context.Context) error {
		if client.SocketExists(client.ConnectorSocketName) {
			return fmt.Errorf("socket %s exists so %s already started or terminated ungracefully",
				client.SocketURL(client.ConnectorSocketName), processName)
		}
		defer func() {
			if perr := derror.PanicToError(recover()); perr != nil {
				dlog.Error(c, perr)
			}
			_ = os.Remove(client.ConnectorSocketName)
		}()

		// Listen on unix domain socket
		listener, err := net.Listen("unix", client.ConnectorSocketName)
		if err != nil {
			return err
		}

		defer func() {
			if perr := derror.PanicToError(recover()); perr != nil {
				dlog.Error(c, perr)
			}
			_ = listener.Close()
			if err != nil {
				dlog.Errorf(c, "Server ended with: %v", err)
			} else {
				dlog.Debug(c, "Server ended")
			}
		}()

		svc := grpc.NewServer()
		rpc.RegisterConnectorServer(svc, s)
		manager.RegisterManagerServer(svc, &s.managerProxy)

		// Need a subgroup here because several services started by incoming gRPC calls run
		// using dgroup.ParentGroup().Go(), and we don't want them to trip
		// ShutdownOnNonError.
		subg := dgroup.NewGroup(c, dgroup.GroupConfig{})

		subg.Go("GracefulStop", func(c context.Context) error {
			<-c.Done()
			dlog.Info(c, "Shutting down")
			svc.GracefulStop()
			return nil
		})
		subg.Go("Serve", func(c context.Context) error {
			s.ctx = c
			err = svc.Serve(listener)
			dlog.Info(c, "Done serving")
			return err
		})

		return subg.Wait()
	})

	err = g.Wait()
	if err != nil {
		dlog.Error(c, err)
	}
	return err
}
