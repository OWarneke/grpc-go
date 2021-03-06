/*
 *
 * Copyright 2019 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package client implementation a full fledged gRPC client for the xDS API
// used by the xds resolver and balancer implementations.
package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	v2corepb "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	v3corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc/credentials/tls/certprovider"
	"google.golang.org/grpc/xds/internal/client/load"

	"google.golang.org/grpc"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/buffer"
	"google.golang.org/grpc/internal/grpclog"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/xds/internal"
	"google.golang.org/grpc/xds/internal/client/bootstrap"
	"google.golang.org/grpc/xds/internal/version"
)

var (
	m = make(map[version.TransportAPI]APIClientBuilder)
)

// RegisterAPIClientBuilder registers a client builder for xDS transport protocol
// version specified by b.Version().
//
// NOTE: this function must only be called during initialization time (i.e. in
// an init() function), and is not thread-safe. If multiple builders are
// registered for the same version, the one registered last will take effect.
func RegisterAPIClientBuilder(b APIClientBuilder) {
	m[b.Version()] = b
}

// getAPIClientBuilder returns the client builder registered for the provided
// xDS transport API version.
func getAPIClientBuilder(version version.TransportAPI) APIClientBuilder {
	if b, ok := m[version]; ok {
		return b
	}
	return nil
}

// BuildOptions contains options to be passed to client builders.
type BuildOptions struct {
	// Parent is a top-level xDS client or server which has the intelligence to
	// take appropriate action based on xDS responses received from the
	// management server.
	Parent UpdateHandler
	// NodeProto contains the Node proto to be used in xDS requests. The actual
	// type depends on the transport protocol version used.
	NodeProto proto.Message
	// Backoff returns the amount of time to backoff before retrying broken
	// streams.
	Backoff func(int) time.Duration
	// Logger provides enhanced logging capabilities.
	Logger *grpclog.PrefixLogger
}

// APIClientBuilder creates an xDS client for a specific xDS transport protocol
// version.
type APIClientBuilder interface {
	// Build builds a transport protocol specific implementation of the xDS
	// client based on the provided clientConn to the management server and the
	// provided options.
	Build(*grpc.ClientConn, BuildOptions) (APIClient, error)
	// Version returns the xDS transport protocol version used by clients build
	// using this builder.
	Version() version.TransportAPI
}

// APIClient represents the functionality provided by transport protocol
// version specific implementations of the xDS client.
//
// TODO: unexport this interface and all the methods after the PR to make
// xdsClient sharable by clients. AddWatch and RemoveWatch are exported for
// v2/v3 to override because they need to keep track of LDS name for RDS to use.
// After the share xdsClient change, that's no longer necessary. After that, we
// will still keep this interface for testing purposes.
type APIClient interface {
	// AddWatch adds a watch for an xDS resource given its type and name.
	AddWatch(ResourceType, string)

	// RemoveWatch cancels an already registered watch for an xDS resource
	// given its type and name.
	RemoveWatch(ResourceType, string)

	// reportLoad starts an LRS stream to periodically report load using the
	// provided ClientConn, which represent a connection to the management
	// server.
	reportLoad(ctx context.Context, cc *grpc.ClientConn, opts loadReportingOptions)

	// Close cleans up resources allocated by the API client.
	Close()
}

// loadReportingOptions contains configuration knobs for reporting load data.
type loadReportingOptions struct {
	loadStore *load.Store
}

// UpdateHandler receives and processes (by taking appropriate actions) xDS
// resource updates from an APIClient for a specific version.
type UpdateHandler interface {
	// NewListeners handles updates to xDS listener resources.
	NewListeners(map[string]ListenerUpdate)
	// NewRouteConfigs handles updates to xDS RouteConfiguration resources.
	NewRouteConfigs(map[string]RouteConfigUpdate)
	// NewClusters handles updates to xDS Cluster resources.
	NewClusters(map[string]ClusterUpdate)
	// NewEndpoints handles updates to xDS ClusterLoadAssignment (or tersely
	// referred to as Endpoints) resources.
	NewEndpoints(map[string]EndpointsUpdate)
}

// ListenerUpdate contains information received in an LDS response, which is of
// interest to the registered LDS watcher.
type ListenerUpdate struct {
	// RouteConfigName is the route configuration name corresponding to the
	// target which is being watched through LDS.
	RouteConfigName string
}

// RouteConfigUpdate contains information received in an RDS response, which is
// of interest to the registered RDS watcher.
type RouteConfigUpdate struct {
	VirtualHosts []*VirtualHost
}

// VirtualHost contains the routes for a list of Domains.
//
// Note that the domains in this slice can be a wildcard, not an exact string.
// The consumer of this struct needs to find the best match for its hostname.
type VirtualHost struct {
	Domains []string
	// Routes contains a list of routes, each containing matchers and
	// corresponding action.
	Routes []*Route
}

// Route is both a specification of how to match a request as well as an
// indication of the action to take upon match.
type Route struct {
	Path, Prefix, Regex *string
	// Indicates if prefix/path matching should be case insensitive. The default
	// is false (case sensitive).
	CaseInsensitive bool
	Headers         []*HeaderMatcher
	Fraction        *uint32
	Action          map[string]uint32 // action is weighted clusters.
}

// HeaderMatcher represents header matchers.
type HeaderMatcher struct {
	Name         string      `json:"name"`
	InvertMatch  *bool       `json:"invertMatch,omitempty"`
	ExactMatch   *string     `json:"exactMatch,omitempty"`
	RegexMatch   *string     `json:"regexMatch,omitempty"`
	PrefixMatch  *string     `json:"prefixMatch,omitempty"`
	SuffixMatch  *string     `json:"suffixMatch,omitempty"`
	RangeMatch   *Int64Range `json:"rangeMatch,omitempty"`
	PresentMatch *bool       `json:"presentMatch,omitempty"`
}

// Int64Range is a range for header range match.
type Int64Range struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// SecurityConfig contains the security configuration received as part of the
// Cluster resource.
type SecurityConfig struct {
	// RootInstanceName identifies the certProvider plugin to be used to fetch
	// root certificates. This instance name will be resolved to the plugin name
	// and its associated configuration from the certificate_providers field of
	// the bootstrap file.
	RootInstanceName string
	// RootCertName is the certificate name to be passed to the plugin (looked
	// up from the bootstrap file) while fetching root certificates.
	RootCertName string
	// IdentityInstanceName identifies the certProvider plugin to be used to
	// fetch identity certificates. This instance name will be resolved to the
	// plugin name and its associated configuration from the
	// certificate_providers field of the bootstrap file.
	IdentityInstanceName string
	// IdentityCertName is the certificate name to be passed to the plugin
	// (looked up from the bootstrap file) while fetching identity certificates.
	IdentityCertName string
	// AcceptedSANs is a list of Subject Alternative Names. During the TLS
	// handshake, the SAN present in the peer certificate is compared against
	// this list, and the handshake succeeds only if a match is found.
	AcceptedSANs []string
}

// ClusterUpdate contains information from a received CDS response, which is of
// interest to the registered CDS watcher.
type ClusterUpdate struct {
	// ServiceName is the service name corresponding to the clusterName which
	// is being watched for through CDS.
	ServiceName string
	// EnableLRS indicates whether or not load should be reported through LRS.
	EnableLRS bool
	// SecurityCfg contains security configuration sent by the xDS server.
	SecurityCfg *SecurityConfig
}

// OverloadDropConfig contains the config to drop overloads.
type OverloadDropConfig struct {
	Category    string
	Numerator   uint32
	Denominator uint32
}

// EndpointHealthStatus represents the health status of an endpoint.
type EndpointHealthStatus int32

const (
	// EndpointHealthStatusUnknown represents HealthStatus UNKNOWN.
	EndpointHealthStatusUnknown EndpointHealthStatus = iota
	// EndpointHealthStatusHealthy represents HealthStatus HEALTHY.
	EndpointHealthStatusHealthy
	// EndpointHealthStatusUnhealthy represents HealthStatus UNHEALTHY.
	EndpointHealthStatusUnhealthy
	// EndpointHealthStatusDraining represents HealthStatus DRAINING.
	EndpointHealthStatusDraining
	// EndpointHealthStatusTimeout represents HealthStatus TIMEOUT.
	EndpointHealthStatusTimeout
	// EndpointHealthStatusDegraded represents HealthStatus DEGRADED.
	EndpointHealthStatusDegraded
)

// Endpoint contains information of an endpoint.
type Endpoint struct {
	Address      string
	HealthStatus EndpointHealthStatus
	Weight       uint32
}

// Locality contains information of a locality.
type Locality struct {
	Endpoints []Endpoint
	ID        internal.LocalityID
	Priority  uint32
	Weight    uint32
}

// EndpointsUpdate contains an EDS update.
type EndpointsUpdate struct {
	Drops      []OverloadDropConfig
	Localities []Locality
}

// Function to be overridden in tests.
var newAPIClient = func(apiVersion version.TransportAPI, cc *grpc.ClientConn, opts BuildOptions) (APIClient, error) {
	cb := getAPIClientBuilder(apiVersion)
	if cb == nil {
		return nil, fmt.Errorf("no client builder for xDS API version: %v", apiVersion)
	}
	return cb.Build(cc, opts)
}

// clientImpl is the real implementation of the xds client. The exported Client
// is a wrapper of this struct with a ref count.
//
// Implements UpdateHandler interface.
// TODO(easwars): Make a wrapper struct which implements this interface in the
// style of ccBalancerWrapper so that the Client type does not implement these
// exported methods.
type clientImpl struct {
	done               *grpcsync.Event
	config             *bootstrap.Config
	cc                 *grpc.ClientConn // Connection to the management server.
	apiClient          APIClient
	watchExpiryTimeout time.Duration

	logger *grpclog.PrefixLogger

	updateCh    *buffer.Unbounded // chan *watcherInfoWithUpdate
	mu          sync.Mutex
	ldsWatchers map[string]map[*watchInfo]bool
	ldsCache    map[string]ListenerUpdate
	rdsWatchers map[string]map[*watchInfo]bool
	rdsCache    map[string]RouteConfigUpdate
	cdsWatchers map[string]map[*watchInfo]bool
	cdsCache    map[string]ClusterUpdate
	edsWatchers map[string]map[*watchInfo]bool
	edsCache    map[string]EndpointsUpdate

	// Changes to map lrsClients and the lrsClient inside the map need to be
	// protected by lrsMu.
	lrsMu      sync.Mutex
	lrsClients map[string]*lrsClient
}

// newWithConfig returns a new xdsClient with the given config.
func newWithConfig(config *bootstrap.Config, watchExpiryTimeout time.Duration) (*clientImpl, error) {
	switch {
	case config.BalancerName == "":
		return nil, errors.New("xds: no xds_server name provided in options")
	case config.Creds == nil:
		return nil, errors.New("xds: no credentials provided in options")
	case config.NodeProto == nil:
		return nil, errors.New("xds: no node_proto provided in options")
	}

	switch config.TransportAPI {
	case version.TransportV2:
		if _, ok := config.NodeProto.(*v2corepb.Node); !ok {
			return nil, fmt.Errorf("xds: Node proto type (%T) does not match API version: %v", config.NodeProto, config.TransportAPI)
		}
	case version.TransportV3:
		if _, ok := config.NodeProto.(*v3corepb.Node); !ok {
			return nil, fmt.Errorf("xds: Node proto type (%T) does not match API version: %v", config.NodeProto, config.TransportAPI)
		}
	}

	dopts := []grpc.DialOption{
		config.Creds,
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    5 * time.Minute,
			Timeout: 20 * time.Second,
		}),
	}

	c := &clientImpl{
		done:               grpcsync.NewEvent(),
		config:             config,
		watchExpiryTimeout: watchExpiryTimeout,

		updateCh:    buffer.NewUnbounded(),
		ldsWatchers: make(map[string]map[*watchInfo]bool),
		ldsCache:    make(map[string]ListenerUpdate),
		rdsWatchers: make(map[string]map[*watchInfo]bool),
		rdsCache:    make(map[string]RouteConfigUpdate),
		cdsWatchers: make(map[string]map[*watchInfo]bool),
		cdsCache:    make(map[string]ClusterUpdate),
		edsWatchers: make(map[string]map[*watchInfo]bool),
		edsCache:    make(map[string]EndpointsUpdate),
		lrsClients:  make(map[string]*lrsClient),
	}

	cc, err := grpc.Dial(config.BalancerName, dopts...)
	if err != nil {
		// An error from a non-blocking dial indicates something serious.
		return nil, fmt.Errorf("xds: failed to dial balancer {%s}: %v", config.BalancerName, err)
	}
	c.cc = cc
	c.logger = prefixLogger((c))
	c.logger.Infof("Created ClientConn to xDS management server: %s", config.BalancerName)

	apiClient, err := newAPIClient(config.TransportAPI, cc, BuildOptions{
		Parent:    c,
		NodeProto: config.NodeProto,
		Backoff:   backoff.DefaultExponential.Backoff,
		Logger:    c.logger,
	})
	if err != nil {
		return nil, err
	}
	c.apiClient = apiClient
	c.logger.Infof("Created")
	go c.run()
	return c, nil
}

// CertProviderConfigs returns the certificate provider configuration from the
// "certificate_providers" field of the bootstrap file. The key in the returned
// map is the plugin_instance_name. Callers must not modify the returned map.
func (c *Client) CertProviderConfigs() map[string]*certprovider.BuildableConfig {
	return c.config.CertProviderConfigs
}

// run is a goroutine for all the callbacks.
//
// Callback can be called in watch(), if an item is found in cache. Without this
// goroutine, the callback will be called inline, which might cause a deadlock
// in user's code. Callbacks also cannot be simple `go callback()` because the
// order matters.
func (c *clientImpl) run() {
	for {
		select {
		case t := <-c.updateCh.Get():
			c.updateCh.Load()
			if c.done.HasFired() {
				return
			}
			c.callCallback(t.(*watcherInfoWithUpdate))
		case <-c.done.Done():
			return
		}
	}
}

// Close closes the gRPC connection to the management server.
func (c *clientImpl) Close() {
	if c.done.HasFired() {
		return
	}
	c.done.Fire()
	// TODO: Should we invoke the registered callbacks here with an error that
	// the client is closed?
	c.apiClient.Close()
	c.cc.Close()
	c.logger.Infof("Shutdown")
}

// ResourceType identifies resources in a transport protocol agnostic way. These
// will be used in transport version agnostic code, while the versioned API
// clients will map these to appropriate version URLs.
type ResourceType int

// Version agnostic resource type constants.
const (
	UnknownResource ResourceType = iota
	ListenerResource
	HTTPConnManagerResource
	RouteConfigResource
	ClusterResource
	EndpointsResource
)

func (r ResourceType) String() string {
	switch r {
	case ListenerResource:
		return "ListenerResource"
	case HTTPConnManagerResource:
		return "HTTPConnManagerResource"
	case RouteConfigResource:
		return "RouteConfigResource"
	case ClusterResource:
		return "ClusterResource"
	case EndpointsResource:
		return "EndpointsResource"
	default:
		return "UnknownResource"
	}
}

// IsListenerResource returns true if the provider URL corresponds to an xDS
// Listener resource.
func IsListenerResource(url string) bool {
	return url == version.V2ListenerURL || url == version.V3ListenerURL
}

// IsHTTPConnManagerResource returns true if the provider URL corresponds to an xDS
// HTTPConnManager resource.
func IsHTTPConnManagerResource(url string) bool {
	return url == version.V2HTTPConnManagerURL || url == version.V3HTTPConnManagerURL
}

// IsRouteConfigResource returns true if the provider URL corresponds to an xDS
// RouteConfig resource.
func IsRouteConfigResource(url string) bool {
	return url == version.V2RouteConfigURL || url == version.V3RouteConfigURL
}

// IsClusterResource returns true if the provider URL corresponds to an xDS
// Cluster resource.
func IsClusterResource(url string) bool {
	return url == version.V2ClusterURL || url == version.V3ClusterURL
}

// IsEndpointsResource returns true if the provider URL corresponds to an xDS
// Endpoints resource.
func IsEndpointsResource(url string) bool {
	return url == version.V2EndpointsURL || url == version.V3EndpointsURL
}
