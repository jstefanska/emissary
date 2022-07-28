package ambex

/**********************************************
 * ambex: Ambassador Experimental ADS server
 *
 * Here's the deal.
 *
 * go-control-plane, several different classes manage this stuff:
 *
 * - The root of the world is a SnapshotCache.
 *   - import github.com/datawire/ambassador/v2/pkg/envoy-control-plane/cache/v2, then refer
 *     to cache.SnapshotCache.
 *   - A collection of internally consistent configuration objects is a
 *     Snapshot (cache.Snapshot).
 *   - Snapshots are collected in the SnapshotCache.
 *   - A given SnapshotCache can hold configurations for multiple Envoys,
 *     identified by the Envoy 'node ID', which must be configured for the
 *     Envoy.
 * - The SnapshotCache can only hold go-control-plane configuration objects,
 *   so you have to build these up to hand to the SnapshotCache.
 * - The gRPC stuff is handled by a Server.
 *   - import github.com/datawire/ambassador/v2/pkg/envoy-control-plane/server, then refer
 *     to server.Server.
 *   - Our runManagementServer (largely ripped off from the go-control-plane
 *     tests) gets this running. It takes a SnapshotCache (cleverly called a
 *     "config" for no reason I understand) and a gRPCServer as arguments.
 *   - _ALL_ the gRPC madness is handled by the Server, with the assistance
 *     of the methods in a callback object.
 * - Once the Server is running, Envoy can open a gRPC stream to it.
 *   - On connection, Envoy will get handed the most recent Snapshot that
 *     the Server's SnapshotCache knows about.
 *   - Whenever a newer Snapshot is added to the SnapshotCache, that Snapshot
 *     will get sent to the Envoy.
 * - We manage the SnapshotCache by loading envoy configuration from
 *   json and/or protobuf files on disk.
 *   - By default when we get a SIGHUP, we reload configuration.
 *   - When passed the -watch argument we reload whenever any file in
 *     the directory changes.
 */

import (
	// standard library

	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/emissary-ingress/emissary/v3/pkg/ambex"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	// third-party libraries
	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	// envoy control plane
	ecp_cache_types "github.com/datawire/ambassador/v2/pkg/envoy-control-plane/cache/types"
	ecp_v2_cache "github.com/datawire/ambassador/v2/pkg/envoy-control-plane/cache/v2"
	ecp_v3_cache "github.com/datawire/ambassador/v2/pkg/envoy-control-plane/cache/v3"
	ecp_log "github.com/datawire/ambassador/v2/pkg/envoy-control-plane/log"
	ecp_v2_server "github.com/datawire/ambassador/v2/pkg/envoy-control-plane/server/v2"
	ecp_v3_server "github.com/datawire/ambassador/v2/pkg/envoy-control-plane/server/v3"

	// Envoy API v2
	// Be sure to import the package of any types that're referenced with "@type" in our
	// generated Envoy config, even if that package is otherwise not used by ambex.
	v2 "github.com/datawire/ambassador/v2/pkg/api/envoy/api/v2"
	v2auth "github.com/datawire/ambassador/v2/pkg/api/envoy/api/v2/auth"
	v2core "github.com/datawire/ambassador/v2/pkg/api/envoy/api/v2/core"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/accesslog/v2"
	v2bootstrap "github.com/datawire/ambassador/v2/pkg/api/envoy/config/bootstrap/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/http/buffer/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/http/ext_authz/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/http/gzip/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/http/lua/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/http/rate_limit/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/http/rbac/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/http/router/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/network/http_connection_manager/v2"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/filter/network/tcp_proxy/v2"
	v2discovery "github.com/datawire/ambassador/v2/pkg/api/envoy/service/discovery/v2"

	// Envoy API v3
	// Be sure to import the package of any types that're referenced with "@type" in our
	// generated Envoy config, even if that package is otherwise not used by ambex.
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/config/accesslog/v3"
	v3bootstrap "github.com/datawire/ambassador/v2/pkg/api/envoy/config/bootstrap/v3"
	v3clusterconfig "github.com/datawire/ambassador/v2/pkg/api/envoy/config/cluster/v3"
	v3core "github.com/datawire/ambassador/v2/pkg/api/envoy/config/core/v3"
	v3endpointconfig "github.com/datawire/ambassador/v2/pkg/api/envoy/config/endpoint/v3"
	v3listenerconfig "github.com/datawire/ambassador/v2/pkg/api/envoy/config/listener/v3"
	v3routeconfig "github.com/datawire/ambassador/v2/pkg/api/envoy/config/route/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/access_loggers/file/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/access_loggers/grpc/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/compression/gzip/compressor/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/buffer/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/compressor/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/ext_authz/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/grpc_stats/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/gzip/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/lua/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/ratelimit/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/rbac/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/response_map/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/http/router/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/network/http_connection_manager/v3"
	_ "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/filters/network/tcp_proxy/v3"
	v3tlsconfig "github.com/datawire/ambassador/v2/pkg/api/envoy/extensions/transport_sockets/tls/v3"
	v3cluster "github.com/datawire/ambassador/v2/pkg/api/envoy/service/cluster/v3"
	v3discovery "github.com/datawire/ambassador/v2/pkg/api/envoy/service/discovery/v3"
	v3endpoint "github.com/datawire/ambassador/v2/pkg/api/envoy/service/endpoint/v3"
	v3listener "github.com/datawire/ambassador/v2/pkg/api/envoy/service/listener/v3"
	v3route "github.com/datawire/ambassador/v2/pkg/api/envoy/service/route/v3"
	v3runtime "github.com/datawire/ambassador/v2/pkg/api/envoy/service/runtime/v3"
	v3secret "github.com/datawire/ambassador/v2/pkg/api/envoy/service/secret/v3"

	// first-party libraries
	"github.com/datawire/dlib/dgroup"
	"github.com/datawire/dlib/dhttp"
	"github.com/datawire/dlib/dlog"
)

type Args struct {
	watch bool

	adsNetwork string
	adsAddress string

	dirs []string

	snapdirPath string
	numsnaps    int
}

func parseArgs(ctx context.Context, rawArgs ...string) (*Args, error) {
	var args Args
	flagset := flag.NewFlagSet("ambex", flag.ContinueOnError)

	flagset.BoolVar(&args.watch, "watch", false, "Watch for file changes")

	// TODO(lukeshu): Consider changing the default here so we don't need to put it in entrypoint.sh
	flagset.StringVar(&args.adsNetwork, "ads-listen-network", "unix", "network for ADS to listen on")
	flagset.StringVar(&args.adsAddress, "ads-listen-address", "/tmp/ambex.sock", "address (on --ads-listen-network) for ADS to listen on")

	var legacyAdsPort uint
	flagset.UintVar(&legacyAdsPort, "ads", 0, "port number for ADS to listen on--deprecated, use --ads-listen-address=:1234 instead")

	if err := flagset.Parse(rawArgs); err != nil {
		return nil, err
	}

	if legacyAdsPort != 0 {
		args.adsAddress = fmt.Sprintf(":%v", legacyAdsPort)
	}

	args.dirs = flagset.Args()
	if len(args.dirs) == 0 {
		args.dirs = []string{"."}
	}

	// ambex logs its own snapshots, separately from the ones provided by the Python
	// side of the world, in $rootdir/snapshots/ambex-#.json, where rootdir is taken
	// from $AMBASSADOR_CONFIG_BASE_DIR if set, else $ambassador_root if set, else
	// whatever, set rootdir to /ambassador.
	snapdirPath := os.Getenv("AMBASSADOR_CONFIG_BASE_DIR")
	if snapdirPath == "" {
		snapdirPath = os.Getenv("ambassador_root")
	}
	if snapdirPath == "" {
		snapdirPath = "/ambassador"
	}
	args.snapdirPath = path.Join(snapdirPath, "snapshots")

	// We'll keep $AMBASSADOR_AMBEX_SNAPSHOT_COUNT snapshots. If unset, or set to
	// something we can't treat as an int, use 30 (which Flynn just made up, so don't
	// be afraid to change it if need be).
	numsnapStr := os.Getenv("AMBASSADOR_AMBEX_SNAPSHOT_COUNT")
	if numsnapStr == "" {
		numsnapStr = "30"
	}
	var err error
	args.numsnaps, err = strconv.Atoi(numsnapStr)
	if (err != nil) || (args.numsnaps < 0) {
		args.numsnaps = 30
		dlog.Errorf(ctx, "Invalid AMBASSADOR_AMBEX_SNAPSHOT_COUNT: %s, using %d", numsnapStr, args.numsnaps)
	}

	return &args, nil
}

// Hasher returns node ID as an ID
type HasherV2 struct {
}

// ID function
func (h HasherV2) ID(node *v2core.Node) string {
	if node == nil {
		return "unknown"
	}
	return node.Id
}

// Hasher returns node ID as an ID
type HasherV3 struct {
}

// ID function
func (h HasherV3) ID(node *v3core.Node) string {
	if node == nil {
		return "unknown"
	}
	return node.Id
}

// end Hasher stuff

// run stuff
// RunManagementServer starts an xDS server at the given port.
func runManagementServer(ctx context.Context, server ecp_v2_server.Server, serverv3 ecp_v3_server.Server, adsNetwork, adsAddress string) error {
	grpcServer := grpc.NewServer()

	lis, err := net.Listen(adsNetwork, adsAddress)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	// register services
	v2discovery.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	v2.RegisterEndpointDiscoveryServiceServer(grpcServer, server)
	v2.RegisterClusterDiscoveryServiceServer(grpcServer, server)
	v2.RegisterRouteDiscoveryServiceServer(grpcServer, server)
	v2.RegisterListenerDiscoveryServiceServer(grpcServer, server)

	v3discovery.RegisterAggregatedDiscoveryServiceServer(grpcServer, serverv3)
	v3secret.RegisterSecretDiscoveryServiceServer(grpcServer, serverv3)
	v3endpoint.RegisterEndpointDiscoveryServiceServer(grpcServer, serverv3)
	v3cluster.RegisterClusterDiscoveryServiceServer(grpcServer, serverv3)
	v3route.RegisterRouteDiscoveryServiceServer(grpcServer, serverv3)
	v3listener.RegisterListenerDiscoveryServiceServer(grpcServer, serverv3)

	dlog.Infof(ctx, "Listening on %s:%s", adsNetwork, adsAddress)

	sc := &dhttp.ServerConfig{
		Handler: grpcServer,
	}
	return sc.Serve(ctx, lis)
}

// Decoders for unmarshalling our config
var decoders = map[string](func([]byte, proto.Message) error){
	".json": protojson.Unmarshal,
	".pb":   prototext.Unmarshal,
}

func isDecodable(name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}

	ext := filepath.Ext(name)
	_, ok := decoders[ext]
	return ok
}

// Not sure if there is a better way to do this, but we cast to this
// so we can call the generated Validate method.
type Validatable interface {
	proto.Message
	Validate() error
}

func Decode(ctx context.Context, name string) (proto.Message, error) {
	any := &anypb.Any{}
	contents, err := ioutil.ReadFile(name)
	if err != nil {
		return nil, err
	}

	ext := filepath.Ext(name)
	decoder := decoders[ext]
	err = decoder(contents, any)
	if err != nil {
		return nil, err
	}

	m, err := any.UnmarshalNew()
	if err != nil {
		return nil, err
	}

	v := m.(Validatable)

	if err := v.Validate(); err != nil {
		return nil, err
	}
	dlog.Infof(ctx, "Loaded file %s", name)
	return v, nil
}

// Get an updated snapshot going.
func update(
	ctx context.Context,
	snapdirPath string,
	numsnaps int,
	config ecp_v2_cache.SnapshotCache,
	configv3 ecp_v3_cache.SnapshotCache,
	generation *int,
	dirs []string,
	edsEndpoints map[string]*v2.ClusterLoadAssignment,
	edsEndpointsV3 map[string]*v3endpointconfig.ClusterLoadAssignment,
	sdsSecretsV2 []*v2auth.Secret,
	sdsSecretsV3 []*v3tlsconfig.Secret,
	fastpathSnapshot *FastpathSnapshot,
	updates chan<- Update,
) error {
	clusters := []ecp_cache_types.Resource{}  // v2.Cluster
	routes := []ecp_cache_types.Resource{}    // v2.RouteConfiguration
	listeners := []ecp_cache_types.Resource{} // v2.Listener
	runtimes := []ecp_cache_types.Resource{}  // discovery.Runtime
	secrets := []ecp_cache_types.Resource{}   // v2auth.Secret

	clustersv3 := []ecp_cache_types.Resource{}  // v3.Cluster
	routesv3 := []ecp_cache_types.Resource{}    // v3.RouteConfiguration
	listenersv3 := []ecp_cache_types.Resource{} // v3.Listener
	runtimesv3 := []ecp_cache_types.Resource{}  // v3.Runtime
	secretsv3 := []ecp_cache_types.Resource{}   // v3.Secret

	var filenames []string

	for _, dir := range dirs {
		files, err := ioutil.ReadDir(dir)
		if err != nil {
			dlog.Warnf(ctx, "Error listing %q: %v", dir, err)
			continue
		}
		for _, file := range files {
			name := file.Name()
			if isDecodable(name) {
				filenames = append(filenames, filepath.Join(dir, name))
			}
		}
	}

	for _, name := range filenames {
		m, e := Decode(ctx, name)
		if e != nil {
			dlog.Warnf(ctx, "%s: %v", name, e)
			continue
		}
		var dst *[]ecp_cache_types.Resource
		switch m.(type) {
		case *v2.Cluster:
			dst = &clusters
		case *v2.RouteConfiguration:
			dst = &routes
		case *v2.Listener:
			dst = &listeners
		case *v2discovery.Runtime:
			dst = &runtimes
		case *v2bootstrap.Bootstrap:
			bs := m.(*v2bootstrap.Bootstrap)
			sr := bs.StaticResources
			for _, lst := range sr.Listeners {
				// When the RouteConfiguration is embedded in the listener, it will cause envoy to
				// go through a complete drain cycle whenever there is a routing change and that
				// will potentially disrupt in-flight requests. By converting all listeners to use
				// RDS rather than inlining their routing configuration, we significantly reduce the
				// set of circumstances where the listener definition itself changes, and this in
				// turn reduces the set of circumstances where envoy has to go through that drain
				// process and disrupt in-flight requests.
				rdsListener, routeConfigs, err := ListenerToRdsListener(lst)
				if err != nil {
					dlog.Errorf(ctx, "Error converting listener to RDS: %+v", err)
					listeners = append(listeners, proto.Clone(lst).(ecp_cache_types.Resource))
					continue
				}
				listeners = append(listeners, rdsListener)
				for _, rc := range routeConfigs {
					// These routes will get included in the configuration snapshot created below.
					routes = append(routes, rc)
				}
			}
			for _, cls := range sr.Clusters {
				clusters = append(clusters, proto.Clone(cls).(ecp_cache_types.Resource))
			}
			continue
		case *v3clusterconfig.Cluster:
			dst = &clustersv3
		case *v3routeconfig.RouteConfiguration:
			dst = &routesv3
		case *v3listenerconfig.Listener:
			dst = &listenersv3
		case *v3runtime.Runtime:
			dst = &runtimesv3
		case *v3bootstrap.Bootstrap:
			bs := m.(*v3bootstrap.Bootstrap)
			sr := bs.StaticResources
			for _, lst := range sr.Listeners {
				// When the RouteConfiguration is embedded in the listener, it will cause envoy to
				// go through a complete drain cycle whenever there is a routing change and that
				// will potentially disrupt in-flight requests. By converting all listeners to use
				// RDS rather than inlining their routing configuration, we significantly reduce the
				// set of circumstances where the listener definition itself changes, and this in
				// turn reduces the set of circumstances where envoy has to go through that drain
				// process and disrupt in-flight requests.
				rdsListener, routeConfigs, err := V3ListenerToRdsListener(lst)
				if err != nil {
					dlog.Errorf(ctx, "Error converting listener to RDS: %+v", err)
					listenersv3 = append(listenersv3, proto.Clone(lst).(ecp_cache_types.Resource))
					continue
				}
				listenersv3 = append(listenersv3, rdsListener)
				for _, rc := range routeConfigs {
					// These routes will get included in the configuration snapshot created below.
					routesv3 = append(routesv3, rc)
				}
			}
			for _, cls := range sr.Clusters {
				clustersv3 = append(clustersv3, proto.Clone(cls).(ecp_cache_types.Resource))
			}
			continue
		default:
			dlog.Warnf(ctx, "Unrecognized resource %s: %v", name, e)
			continue
		}
		*dst = append(*dst, m.(ecp_cache_types.Resource))
	}

	if fastpathSnapshot != nil && fastpathSnapshot.Snapshot != nil {
		for _, lst := range fastpathSnapshot.Snapshot.Resources[ecp_cache_types.Listener].Items {
			listeners = append(listeners, lst.Resource)
		}
		for _, route := range fastpathSnapshot.Snapshot.Resources[ecp_cache_types.Route].Items {
			routes = append(routes, route.Resource)
		}
		for _, clu := range fastpathSnapshot.Snapshot.Resources[ecp_cache_types.Cluster].Items {
			clusters = append(clusters, clu.Resource)
		}
		// We intentionally omit endpoints and secrets since those are carried separately.
	}

	// The configuration data that reaches us here arrives via two parallel paths that race each
	// other. The endpoint data comes in realtime directly from the golang watcher in the entrypoint
	// package. The cluster configuration comes from the python code. Either one can win which means
	// we might at times see endpoint data with no corresponding cluster and we might also see
	// clusters with no corresponding endpoint data. Both of these circumstances should be
	// transient.
	//
	// To produce a consistent configuration we do an outer join operation on the endpoint and
	// cluster configuration that we have at this moment. If there is no endpoint information for a
	// given cluster, we will synthesize an empty ClusterLoadAssignment.
	//
	// Note that a cluster not existing is very different to envoy than a cluster existing but
	// having an empty ClusterLoadAssignment. When envoy first discovers clusters it goes through a
	// warmup process to be sure the cluster is properly bootstrapped before routing traffic to
	// it. See here for more details:
	//
	// https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/cluster_manager.html?highlight=cluster%20warming
	//
	// For this reason if there is no endpoint data for the cluster we will synthesize an empty
	// ClusterLoadAssignment rather than filtering out the cluster. This avoids triggering the
	// warmup sequence in scenarios where the endpoint data for a cluster is really flapping into
	// and out of existence. In that circumstance we want to faithfully relay to envoy that the
	// cluster exists but currently has no endpoints.
	endpoints := JoinEdsClusters(ctx, clusters, edsEndpoints)
	endpointsv3 := JoinEdsClustersV3(ctx, clustersv3, edsEndpointsV3)

	for _, secretv2 := range sdsSecretsV2 {
		dlog.Warnf(ctx, "Updating with V2 secret %s", secretv2.Name)
		secrets = append(secrets, secretv2)
	}

	dlog.Info(ctx, "&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&")
	dlog.Infof(ctx, "sdsSecretsv3 before range : %v", sdsSecretsV3)
	dlog.Info(ctx, "&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&&")

	for _, secretv3 := range sdsSecretsV3 {
		dlog.Warnf(ctx, "Updating with V3 secret %s", secretv3.Name)

		dlog.Infof(ctx, "GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG")
		dlog.Info(ctx, secretv3)
		dlog.Infof(ctx, "GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG")
		secretsv3 = append(secretsv3, secretv3)
	}

	dlog.Infof(ctx, "!_!_!_!__!_!_!_!_!_!_!_!_!_!__!_!_!_!_!_!_!_!_!__!_!_!_!_!_!_!_!__!_!_!_!_!_!_!!__!_!_!_!")
	dlog.Info(ctx, secrets)
	dlog.Info(ctx, secretsv3)
	dlog.Infof(ctx, "!_!_!_!__!_!_!_!_!_!_!_!_!_!__!_!_!_!_!_!_!_!_!__!_!_!_!_!_!_!_!__!_!_!_!_!_!_!!__!_!_!_!")

	// Create a new configuration snapshot from everything we have just loaded from disk.
	curgen := *generation
	*generation++

	version := fmt.Sprintf("v%d", curgen)
	snapshot := ecp_v2_cache.NewSnapshot(
		version,
		endpoints,
		clusters,
		routes,
		listeners,
		runtimes,
		secrets,
	)

	if err := snapshot.Consistent(); err != nil {
		bs, _ := json.Marshal(snapshot)
		dlog.Errorf(ctx, "V2 Snapshot inconsistency: %v: %s", err, bs)
		return nil // TODO: should we return the error, rather than just logging it?
	}

	snapshotv3 := ecp_v3_cache.NewSnapshot(
		version,
		endpointsv3,
		clustersv3,
		routesv3,
		listenersv3,
		runtimesv3,
		secretsv3,
	)

	if err := snapshotv3.Consistent(); err != nil {
		bs, _ := json.Marshal(snapshotv3)
		dlog.Errorf(ctx, "V3 Snapshot inconsistency: %v: %s", err, bs)
		return nil // TODO: should we return the error, rather than just logging it?
	}

	time="2022-07-28 15:41:53.1450" level=info msg="snapshot v3 : {[{v8 map[]}" +
		" {v8 map[cluster_127_0_0_1_8877_emissary:{name:\"cluster_127_0_0_1_8877_emissary\" alt_stat_name:\"127_0_0_1_8877\" type:STRICT_DNS connect_timeout:{seconds:3} load_assignment:{cluster_name:\"cluster_127_0_0_1_8877_emissary\" endpoints:{lb_endpoints:{endpoint:{address:{socket_address:{address:\"127.0.0.1\" port_value:8877}}}}}} dns_lookup_family:V4_ONLY <nil>}]} " +
		"{v8 map[emissary-ingress-listener-8080-routeconfig-0:{name:\"emissary-ingress-listener-8080-routeconfig-0\" virtual_hosts:{name:\"emissary-ingress-listener-8080-*\" domains:\"*\" routes:{match:{prefix:\"/ambassador/v0/check_ready\" case_sensitive:{value:true} runtime_fraction:{default_value:{numerator:100} runtime_key:\"routing.traffic_shift.cluster_127_0_0_1_8877_emissary\"} headers:{name:\"x-forwarded-proto\" exact_match:\"https\"}} route:{cluster:\"cluster_127_0_0_1_8877_emissary\" prefix_rewrite:\"/ambassador/v0/check_ready\" timeout:{seconds:10}}} routes:{match:{prefix:\"/ambassador/v0/check_ready\" case_sensitive:{value:true} runtime_fraction:{default_value:{numerator:100} runtime_key:\"routing.traffic_shift.cluster_127_0_0_1_8877_emissary\"}} redirect:{https_redirect:true}} routes:{match:{prefix:\"/ambassador/v0/check_alive\" case_sensitive:{value:true} runtime_fraction:{default_value:{numerator:100} runtime_key:\"routing.traffic_shift.cluster_127_0_0_1_8877_emissary\"} headers:{name:\"x-forwarded-proto\" exact_match:\"https\"}} route:{cluster:\"cluster_127_0_0_1_8877_emissary\" prefix_rewrite:\"/ambassador/v0/check_alive\" timeout:{seconds:10}}} routes:{match:{prefix:\"/ambassador/v0/check_alive\" case_sensitive:{value:true} runtime_fraction:{default_value:{numerator:100} runtime_key:\"routing.traffic_shift.cluster_127_0_0_1_8877_emissary\"}} redirect:{https_redirect:true}} routes:{match:{prefix:\"/ambassador/v0/\" case_sensitive:{value:true} runtime_fraction:{default_value:{numerator:100} runtime_key:\"routing.traffic_shift.cluster_127_0_0_1_8877_emissary\"} headers:{name:\"x-forwarded-proto\" exact_match:\"https\"}} route:{cluster:\"cluster_127_0_0_1_8877_emissary\" prefix_rewrite:\"/ambassador/v0/\" timeout:{seconds:10}}} routes:{match:{prefix:\"/ambassador/v0/\" case_sensitive:{value:true} runtime_fraction:{default_value:{numerator:100} runtime_key:\"routing.traffic_shift.cluster_127_0_0_1_8877_emissary\"}} redirect:{https_redirect:true}}} <nil>}]} " +
		"{v8 map[emissary-ingress-listener-8080:{name:\"emissary-ingress-listener-8080\" address:{socket_address:{address:\"0.0.0.0\" port_value:8080}} filter_chains:{filter_chain_match:{} filters:{name:\"envoy.filters.network.http_connection_manager\" typed_config:{[type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager]:{stat_prefix:\"ingress_http\" rds:{config_source:{ads:{} resource_api_version:V3} route_config_name:\"emissary-ingress-listener-8080-routeconfig-0\"} http_filters:{name:\"envoy.filters.http.cors\"} http_filters:{name:\"envoy.filters.http.router\"} http_protocol_options:{} server_name:\"envoy\" access_log:{name:\"envoy.access_loggers.file\" typed_config:{[type.googleapis.com/envoy.extensions.access_loggers.file.v3.FileAccessLog]:{path:\"/dev/fd/1\" log_format:{text_format_source:{inline_string:\"ACCESS [%START_TIME%] \\\"%REQ(:METHOD)% %REQ(X-ENVOY-ORIGINAL-PATH?:PATH)% %PROTOCOL%\\\" %RESPONSE_CODE% %RESPONSE_FLAGS% %BYTES_RECEIVED% %BYTES_SENT% %DURATION% %RESP(X-ENVOY-UPSTREAM-SERVICE-TIME)% \\\"%REQ(X-FORWARDED-FOR)%\\\" \\\"%REQ(USER-AGENT)%\\\" \\\"%REQ(X-REQUEST-ID)%\\\" \\\"%REQ(:AUTHORITY)%\\\" \\\"%UPSTREAM_HOST%\\\"\\n\"}}}}} use_remote_address:{value:true} normalize_path:{value:true}}}}} <nil>}]} " +
		"{v8 map[tls-cert.emissary:{name:\"tls-cert.emissary\" tls_certificate:{certificate_chain:{inline_bytes:\"-----BEGIN CERTIFICATE-----\\nMIIEsDCCApgCCQD/WNUQQtHflzANBgkqhkiG9w0BAQsFADAaMRgwFgYDVQQDDA9h\\nbWJhc3NhZG9yLWNlcnQwHhcNMjIwNzI1MTk0OTI1WhcNMjIwODI0MTk0OTI1WjAa\\nMRgwFgYDVQQDDA9hbWJhc3NhZG9yLWNlcnQwggIiMA0GCSqGSIb3DQEBAQUAA4IC\\nDwAwggIKAoICAQC8Lo1RanFHlVyYnQ1fgJhx+Ojg/8/5u54Dr8jVkzxOjTeNi9Cc\\nu8WlBiS096l+z4s6YtcapT0ep6jLzaoFlCheTXJLfICk0WwLrHp2+wlHMfMS6Mcj\\nqko8G1XQU9VPhogiRGEd31zlRO/pIkYUo2WZ3yRNJShBUopZSi2DVF1M7uEZGaEC\\n9ks60Lt076AKNkw7LQvwOLfAE7e4yGyHjX3zAVihgIFU7spLYizc1O3VkgSxV8Rh\\nGyrlZVSMzedLas7aeHQxlHRYfe99/tQQz/bXJMm9ib82ncx0C3asXsSpAIcZVACU\\nIaecuttIgn3J13ebrGEKGDagdZRKB7G51d1BB57+2ZNNlimYYIA5ZL95+0ahEDtT\\nnwX8dVlHfM8EFI44Ed1N4KXqEzclsuycrkQX0KBeg/tQbk8sDJu+7VL88+4P12k7\\ng1EL/hducRXNzIqsNIq3FrToOEkKTwaaw/SP4wYcul/BsICvt/eRLNfEEDwLPhol\\nJB3h96MYo6lk8LbxzWy73hBrVgREsMGnCAl1zj2RaNoxjebHE2qWRPYFD+WLX9z+\\n4gEdKI/9UQ3GEtGFEQ09jBND2QG3x3iciPS0xRaMiOsC7DvXBBXcCeq+sw+F0ifG\\nN835khgEQEp14lcUbBcj8tunTle65Nt0ZGcFU4v+xCq7X1dK2aP5Jxr8qQIDAQAB\\nMA0GCSqGSIb3DQEBCwUAA4ICAQBGLPwUFXCSN0mdcwZFcppkjtwFwIZ/50vcOsxu\\nfl1FDUM7Y27g1R/j2Zm67TKMsc8aVY8Zrb7oVWWPM+ter+sKcUJSgW4Ttbqap3kB\\nJ6aMdVRGs4TCGeIRtBalIAC/uTISIqPfpruN8FkbKICvM7zAB/7CHEf8fOX7pxB6\\nI7TcMUm7VWrg/AT2EOw78yVlhHMutsVJrArmQeeL1URcaMgsQ3nvks6rmr1USDcg\\n/H2zmqQ5q+UbBWBA+SkANxiicLEDjG3040oBTThWT8iXl8nLYPmE/omTSD8Jn8dw\\n4M6CwwZszKb2hG3BM/vqM5bwZA7yC/ryVHh3H/Zq9MzXL0uQY/RJ7kCOegwyWXfW\\nIoqYG6Y3wdNF6c1elhYVS1yLqjoEKe53I26kPN+yIyD+4eoqfzl6g77TsNZn3HB5\\n5PEyaRRV+HjsR5J8hX6gbZWa2CJzMZcPOQP2iGVdWmDGr4jlE7G0GOuRYwB6r9nQ\\npwf3sUTcsz+lvZ+vVdlbd3hy7nvewZFWhECAH9kt+5lodDQwnDbsUTdhpdexxYSg\\nHtBAuVJFGQUraYDzTcN4xIq2BS8eNUoVfhzJ12CvWnAznRkq5+djGxOtGk2G1XsW\\nTyJZWozhvMD+Y2+Rb+5pZWMZCCvAVRICpuz2YBDuNrmXzu+7F5ZRHW6qlM5Afmt7\\nnWqjMA==\\n-----END CERTIFICATE-----\\n\"} private_key:{inline_bytes:\"-----BEGIN PRIVATE KEY-----\\nMIIJQgIBADANBgkqhkiG9w0BAQEFAASCCSwwggkoAgEAAoICAQC8Lo1RanFHlVyY\\nnQ1fgJhx+Ojg/8/5u54Dr8jVkzxOjTeNi9Ccu8WlBiS096l+z4s6YtcapT0ep6jL\\nzaoFlCheTXJLfICk0WwLrHp2+wlHMfMS6Mcjqko8G1XQU9VPhogiRGEd31zlRO/p\\nIkYUo2WZ3yRNJShBUopZSi2DVF1M7uEZGaEC9ks60Lt076AKNkw7LQvwOLfAE7e4\\nyGyHjX3zAVihgIFU7spLYizc1O3VkgSxV8RhGyrlZVSMzedLas7aeHQxlHRYfe99\\n/tQQz/bXJMm9ib82ncx0C3asXsSpAIcZVACUIaecuttIgn3J13ebrGEKGDagdZRK\\nB7G51d1BB57+2ZNNlimYYIA5ZL95+0ahEDtTnwX8dVlHfM8EFI44Ed1N4KXqEzcl\\nsuycrkQX0KBeg/tQbk8sDJu+7VL88+4P12k7g1EL/hducRXNzIqsNIq3FrToOEkK\\nTwaaw/SP4wYcul/BsICvt/eRLNfEEDwLPholJB3h96MYo6lk8LbxzWy73hBrVgRE\\nsMGnCAl1zj2RaNoxjebHE2qWRPYFD+WLX9z+4gEdKI/9UQ3GEtGFEQ09jBND2QG3\\nx3iciPS0xRaMiOsC7DvXBBXcCeq+sw+F0ifGN835khgEQEp14lcUbBcj8tunTle6\\n5Nt0ZGcFU4v+xCq7X1dK2aP5Jxr8qQIDAQABAoICACegwf+4I5zIdlRpFkzbPr8Z\\nhKkFFd3oRZFPLkLTm6qLEHzNSaQH4soqrgYx9U6QFCi6ccPZsjJHakNozvoXxhx9\\nwKjm0gxjUZZvvuZZXQ3SFfcQcAcxsCpSM1MIpG33zakxryXkSKK7Tq5QmmN0OUwa\\n8oEQZxbCdAHznfv/AxrVXy+y9vYxuYvsMt2gLJqZ7pmzlcUDnKZlQJY4FqrBABnx\\nWaHA9vtUweL4DKyAZolIE5bo/aDZYMIoAZMZNTDrkB3vqcBbG9kaxBlLVYso0Dlk\\nNNumK08sEccKNvZ7inxwcQLDbs/C+pIFw3HUkPq1VinBNjoy+SX3PMAOOL4CrHHN\\n36UlvBZFbFI7G97anDEQmU3mCW7YK689CxbDoTa7E+pRits0Jec3tQiYQRNnavfJ\\nUlwHxxwZrnRkvU7N7tatwuAjcFaqabsWQZ8gqsTmB8YsQHywAIPTv39sZV3ElKKO\\nxeM9BAHX2Q1lpoL3CDR1XFLLsxRXggrjZsQc4K/xOGM4Bve0omDbmE/fLczAd1KT\\n/ufmpoGwJAednvZFkh3BmZ5FPo2t5k6cPSlww1cYz5CzOgo11wyOKK4xn60ZRPmX\\nVBU0efIGwtRwqQorXyU4juZKMtikoJPiqaQlbZnyyITEdNFoYxZVP5kz0AOiKzVw\\nSAnw0Ko8dIv7WjKZqwUBAoIBAQDdwABhQf5ljfmymLMdrx5MdDnHicwAWn2NIluG\\n6DfmijmzKYkouYiDM7Cp2ecf0dYNMoKz7fAbNNMsrWD/XeHlNFtONyDAetiutfGr\\ntWQ+n4Mak1JUzJlt8k+Kq4hoGsCyKmv7wavJZd/k4nVm5ASozAtL7Jbko2gLzLKF\\nqV63+BgH1azabnm3hKaylfI2dSHmE+PAswYeG36ACAaAvb1sXT/kBDrZhqeyCrRk\\napM1vSNgpHv5tM7BDQ5kTBAHbvYWvHQqzlmL/4uM80Z5Dk/EiImT5g5zqzNtS9LT\\n4stAVf7YcCi7HTN+2EU3kipKW1bE9Ai396oBht2NSO0LaWgJAoIBAQDZP0PRryaJ\\n6YPeE0PAJ0XMa3t1mGqF7wuENXD7vrlnAYArhW/RXwJuFP/RCqDFn7L3AyM069Kc\\neVZPsll944JwzeOIwp7uFiZAZfgeeKCYFvhtaGXEwqMcaezL8kPBvC70KAVZoVDW\\nOgtfof+H8DuU5/Sq5R6bXWuqxc5jzhx7jeQFnnu9T3IS94e+N/NAPl5+ICVYfpFt\\nf8rDAlRTCvnnUs0EQxxbW3QPx6RWTIBg71Q54iwaYwNhy+Iy0XU+/ASuSKYckv9S\\nFcOVAl37+letI3PBSXlzM2c9m1/oK8z6qV9inYrOANiHBtfepGVPbwVlZ7XG2cUp\\n0wQIS236KdehAoIBAGrcQdKcmNmbp37Xc0mUwEcbo1RzJSnucZ53th+2/GlOpyud\\nkolbj1uap0HSzjxZRIurkazfttuFg1Z+XsXxGhD4QB/bFR9sk8rtxv52NU6wU9+U\\nqxJOXonLQWWT7/IrIj05kDwMQpqVS2Xjc6wXqd8mJTN7WePokxVMmaMr0YdaOUpd\\nQCUrI+C6PhLFi6mOnL1hTFnqa8JKgZV1SJz10rjL3yQkna+B4oI8Al55tWMqOEg0\\nO8JDzXbesdbHiNnsMQ8X1eti8Wgzx64u9gEUUoBlQ9dQABrhn7DF311YZ5YjWuI2\\neophMhWoZwK2n0ah6ZiT+TeeVADUWyPgxsq/CvECggEBAM/bKBhHbKgnNHQWGoyP\\nBsk/TTCGU+UxgeNM/nzfTqSvWuJJGWNJdBm/HcP0Vz6+PYz5A4Zu22O69EnsapN4\\nSiWD2F/RVh6WMqMK7Qq6f20FDUwgxr4rO8TD1DyN3yZN1t7kK7pGDgHqJ1eot8Ro\\npZkjFYUtEsuz5n/OIlp7qeTNLY3yxJGsoYhkLQxim+qrwd0LBLBW6O3Lh0RtxiBu\\nMAyTvHSHgWonZjRy31lyeKJGWl/HdksI0QOQ/yuAHJ3WouPdvpE4U/FFwYugaCve\\nqmSNtwMIOZtvYygesPSCc+yggRLpiRTw2jrW4VO0FvqZTqBnShSn52zM6N7GzMK9\\npgECggEAXRDeZ1MRGR9wrNP4HktyYmn04spW+I3Zy/UzGQGjr0mFhH6oQZ++kvhQ\\nIZVSu7Nc88Xl7+MwWcdFh7Ags1SXab0eIyRzVl1KCVIP1n3tmPdSe8Ck2HpRi+PX\\ncgZy1jcl3MA87TnsT1Ptydcth2V+AAK/QM7L3YiB6Jw2B1odEsz1qdyv3/vaDt5y\\nwV5NHeBeP5Nn9PblN3ZpOWuBVidvxMGXM2juBTdeQwOnyUdiK8524u0hkZ5QnNCN\\n3KzG+wSkOObIXWP8MB6LVzZfey85vSVNVPQB+qUWsP3yGBCfK8PSeWU7sJnO8atx\\nWei4PSpUZh51A/qMXOx3IdYgcL6Zlw==\\n-----END PRIVATE KEY-----\\n\"}} <nil>}]} " +
		"{v8 map[]} " +
		"{v8 map[]}]}" func=github.com/datawire/ambassador/v2/pkg/ambex.update file="/go/pkg/ambex/main.go:540" CMD=entrypoint PID=1 THREAD=/ambex/main-loop
	time="2022-07-28 15:41:53.1453" level=info msg="+++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++==" func=github.com/datawire/ambassador/v2/pkg/ambex.update file="/go/pkg/ambex/main.go:541" CMD=entrypoint PID=1 THREAD=/ambex/main-loop



	dlog.Info(ctx, "+++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++==")
	dlog.Infof(ctx, "snapshot v2 : %v", snapshot)
	dlog.Infof(ctx, "snapshot v3 : %v", snapshotv3)
	dlog.Info(ctx, "+++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++==")

	// This used to just directly update envoy. Since we want ratelimiting, we now send an
	// Update object down the channel with a function that knows how to do the update if/when
	// the ratelimiting logic decides.

	dlog.Debugf(ctx, "Created snapshot %s", version)
	dumpSnapshot(ctx, snapdirPath, numsnaps, curgen, &snapshot, &snapshotv3)

	update := Update{version, func() error {
		dlog.Debugf(ctx, "Accepting snapshot %s", version)

		err := config.SetSnapshot("test-id", snapshot)
		if err != nil {
			return fmt.Errorf("V2 Snapshot error %q for %+v", err, snapshot)
		}

		err = configv3.SetSnapshot("test-id", snapshotv3)
		if err != nil {
			return fmt.Errorf("V3 Snapshot error %q for %+v", err, snapshotv3)
		}

		return nil
	}}
	// We also need to pay attention to contexts here so we can shutdown properly. If we didn't
	// have the context portion, the ratelimit goroutine could shutdown first and we could end
	// up blocking here and never shutting down.
	select {
	case updates <- update:
	case <-ctx.Done():
	}
	return nil
}

type logAdapterBase struct {
	prefix string
}

type logAdapterV2 struct {
	logAdapterBase
}

var _ ecp_v2_server.Callbacks = logAdapterV2{}
var _ ecp_log.Logger = logAdapterV2{}

type logAdapterV3 struct {
	logAdapterBase
}

var _ ecp_v3_server.Callbacks = logAdapterV3{}
var _ ecp_log.Logger = logAdapterV3{}

// Debugf implements ecp_log.Logger.
func (l logAdapterBase) Debugf(format string, args ...interface{}) {
	dlog.Infof(context.Background(), format, args...)
}

// Infof implements ecp_log.Logger.
func (l logAdapterBase) Infof(format string, args ...interface{}) {
	dlog.Infof(context.TODO(), format, args...)
}

// Warnf implements ecp_log.Logger.
func (l logAdapterBase) Warnf(format string, args ...interface{}) {
	dlog.Warnf(context.TODO(), format, args...)
}

// Errorf implements ecp_log.Logger.
func (l logAdapterBase) Errorf(format string, args ...interface{}) {
	dlog.Errorf(context.TODO(), format, args...)
}

// OnStreamOpen implements ecp_v2_server.Callbacks and ecp_v3_server.Callbacks.
func (l logAdapterBase) OnStreamOpen(ctx context.Context, sid int64, stype string) error {
	dlog.Infof(ctx, "%v Stream open[%v]: %v", l.prefix, sid, stype)
	return nil
}

// OnStreamClosed implements ecp_v2_server.Callbacks and ecp_v3_server.Callbacks.
func (l logAdapterBase) OnStreamClosed(sid int64) {
	dlog.Infof(context.Background(), "%v Stream closed[%v]", l.prefix, sid)
}

// OnStreamRequest implements ecp_v2_server.Callbacks.
func (l logAdapterV2) OnStreamRequest(sid int64, req *v2.DiscoveryRequest) error {
	dlog.Infof(context.Background(), "V2 Stream request[%v] for type %s: %s", sid, req.TypeUrl, strings.Join(req.ResourceNames, ","))
	return nil
}

// OnStreamRequest implements ecp_v3_server.Callbacks.
func (l logAdapterV3) OnStreamRequest(sid int64, req *v3discovery.DiscoveryRequest) error {
	dlog.Infof(context.Background(), "V3 Stream request[%v] for type %s: %s", sid, req.TypeUrl, strings.Join(req.ResourceNames, ","))
	return nil
}

// OnStreamResponse implements ecp_v2_server.Callbacks.
func (l logAdapterV2) OnStreamResponse(sid int64, req *v2.DiscoveryRequest, res *v2.DiscoveryResponse) {
	dlog.Infof(context.Background(), "V2 Stream response[%v] for type %s: returning %d resources", sid, res.TypeUrl, len(res.Resources))
}

// OnStreamResponse implements ecp_v3_server.Callbacks.
func (l logAdapterV3) OnStreamResponse(sid int64, req *v3discovery.DiscoveryRequest, res *v3discovery.DiscoveryResponse) {
	dlog.Infof(context.Background(), "V3 Stream response[%v] for type %s: returning %d resources: %v", sid, res.TypeUrl, len(res.Resources), res.Resources)
}

// OnFetchRequest implements ecp_v2_server.Callbacks.
func (l logAdapterV2) OnFetchRequest(ctx context.Context, r *v2.DiscoveryRequest) error {
	dlog.Infof(ctx, "V2 Fetch request: %v", r)
	return nil
}

// OnFetchRequest implements ecp_v3_server.Callbacks.
func (l logAdapterV3) OnFetchRequest(ctx context.Context, r *v3discovery.DiscoveryRequest) error {
	dlog.Infof(ctx, "V3 Fetch request: %v", r)
	return nil
}

// OnFetchResponse implements ecp_v2_server.Callbacks.
func (l logAdapterV2) OnFetchResponse(req *v2.DiscoveryRequest, res *v2.DiscoveryResponse) {
	dlog.Infof(context.Background(), "V2 Fetch response: %v -> %v", req, res)
}

// OnFetchResponse implements ecp_v3_server.Callbacks.
func (l logAdapterV3) OnFetchResponse(req *v3discovery.DiscoveryRequest, res *v3discovery.DiscoveryResponse) {
	dlog.Infof(context.Background(), "V3 Fetch response: %v -> %v", req, res)
}

func Main(
	ctx context.Context,
	Version string,
	getUsage MemoryGetter,
	fastpathCh <-chan *FastpathSnapshot,
	rawArgs ...string,
) error {
	args, err := parseArgs(ctx, rawArgs...)
	if err != nil {
		return err
	}

	dlog.Infof(ctx, "Ambex %s starting, snapdirPath %s", Version, args.snapdirPath)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if args.watch {
		for _, d := range args.dirs {
			if err := watcher.Add(d); err != nil {
				return err
			}
		}
	}

	// The golang signal package does not block when it writes to the channel. We therefore need a
	// nonzero buffer for the channel to minimize the possiblity that we miss out on a signal that
	// comes in while we are doing work and not reading from the channel. To minimize the chance
	// of that happening we will choose a buffer size of 100. That may well be overkill, but
	// better to not have to consider the possibility that we lose a signal.
	sigCh := make(chan os.Signal, 100)
	signal.Notify(sigCh, syscall.SIGHUP)
	defer func() { signal.Stop(sigCh) }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	config := ecp_v2_cache.NewSnapshotCache(true, HasherV2{}, logAdapterV2{logAdapterBase{"V2"}})
	configv3 := ecp_v3_cache.NewSnapshotCache(true, HasherV3{}, logAdapterV3{logAdapterBase{"V3"}})
	server := ecp_v2_server.NewServer(ctx, config, logAdapterV2{logAdapterBase{"V2"}})
	serverv3 := ecp_v3_server.NewServer(ctx, configv3, logAdapterV3{logAdapterBase{"V3"}})

	grp := dgroup.NewGroup(ctx, dgroup.GroupConfig{})

	grp.Go("management-server", func(ctx context.Context) error {
		return runManagementServer(ctx, server, serverv3, args.adsNetwork, args.adsAddress)
	})

	pid := os.Getpid()
	file := "ambex.pid"
	if err := ioutil.WriteFile(file, []byte(fmt.Sprintf("%v", pid)), 0644); err != nil {
		dlog.Warn(ctx, err)
	} else {
		ctx := dlog.WithField(ctx, "pid", pid)
		ctx = dlog.WithField(ctx, "file", file)
		dlog.Info(ctx, "Wrote PID")
	}

	updates := make(chan Update)
	grp.Go("updater", func(ctx context.Context) error {
		return Updater(ctx, updates, getUsage)
	})
	grp.Go("main-loop", func(ctx context.Context) error {
		generation := 0
		var fastpathSnapshot *FastpathSnapshot
		edsEndpoints := map[string]*v2.ClusterLoadAssignment{}
		edsEndpointsV3 := map[string]*v3endpointconfig.ClusterLoadAssignment{}
		var sdsSecretsV2 []*v2auth.Secret
		var sdsSecretsV3 []*v3tlsconfig.Secret

		// We always start by updating with a totally empty snapshot.
		//
		// XXX This seems questionable: why do we do this? Envoy isn't currently started until
		// we have a real configuration...
		err = update(
			ctx,
			args.snapdirPath,
			args.numsnaps,
			config,
			configv3,
			&generation,
			args.dirs,
			edsEndpoints,
			edsEndpointsV3,
			sdsSecretsV2,
			sdsSecretsV3,
			fastpathSnapshot,
			updates,
		)
		if err != nil {
			return err
		}

		// This is the main loop where the magic happens.
		for {

			select {
			case <-sigCh:

				dlog.Info(ctx, "-------------------------------------------------------------")
				dlog.Info(ctx, "sigCH start")
				dlog.Infof(ctx, "secrets v2 : %v", sdsSecretsV2)
				dlog.Infof(ctx, "secrets v3 : %v", sdsSecretsV3)
				dlog.Info(ctx, "sigCH end")
				dlog.Info(ctx, "-------------------------------------------------------------")

				err := update(
					ctx,
					args.snapdirPath,
					args.numsnaps,
					config,
					configv3,
					&generation,
					args.dirs,
					edsEndpoints,
					edsEndpointsV3,
					sdsSecretsV2,
					sdsSecretsV3,
					fastpathSnapshot,
					updates,
				)
				if err != nil {
					return err
				}
			case fpSnap := <-fastpathCh:

				// Fastpath update. Grab new endpoints and update.
				if fpSnap.Endpoints != nil {
					edsEndpoints = fpSnap.Endpoints.ToMap_v2()
					edsEndpointsV3 = fpSnap.Endpoints.ToMap_v3()
				}
				if fpSnap.Secrets != nil {
					sdsSecretsV2 = fpSnap.Secrets.ToV2List(ctx, fpSnap.ValidationGroups)
					sdsSecretsV3 = fpSnap.Secrets.ToV3List(ctx, fpSnap.ValidationGroups)
				}

				dlog.Info(ctx, "-------------------------------------------------------------")
				dlog.Info(ctx, "fastPathCH start")
				dlog.Infof(ctx, "secrets v2 : %v", sdsSecretsV2)
				dlog.Infof(ctx, "secrets v3 : %v", sdsSecretsV3)
				dlog.Info(ctx, "fastPathCH end")
				dlog.Info(ctx, "-------------------------------------------------------------")

				fastpathSnapshot = fpSnap
				err := update(
					ctx,
					args.snapdirPath,
					args.numsnaps,
					config,
					configv3,
					&generation,
					args.dirs,
					edsEndpoints,
					edsEndpointsV3,
					sdsSecretsV2,
					sdsSecretsV3,
					fastpathSnapshot,
					updates,
				)
				if err != nil {
					return err
				}
			case <-watcher.Events:
				// Non-fastpath update. Just update.

				dlog.Info(ctx, "-------------------------------------------------------------")
				dlog.Info(ctx, "watcherEvents start")
				dlog.Infof(ctx, "secrets v2 : %v", sdsSecretsV2)
				dlog.Infof(ctx, "secrets v3 : %v", sdsSecretsV3)
				dlog.Info(ctx, "watcherEvents end")
				dlog.Info(ctx, "-------------------------------------------------------------")
				err := update(
					ctx,
					args.snapdirPath,
					args.numsnaps,
					config,
					configv3,
					&generation,
					args.dirs,
					edsEndpoints,
					edsEndpointsV3,
					sdsSecretsV2,
					sdsSecretsV3,
					fastpathSnapshot,
					updates,
				)
				if err != nil {
					return err
				}
			case err := <-watcher.Errors:
				// Something went wrong, so scream about that.
				dlog.Warnf(ctx, "Watcher error: %v", err)
			case <-ctx.Done():
				return nil
			}
		}
	})

	return grp.Wait()
}
