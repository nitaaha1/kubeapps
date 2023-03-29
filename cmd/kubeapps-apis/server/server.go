// Copyright 2021-2022 the Kubeapps contributors.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"reflect"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	grpchealth "github.com/bufbuild/connect-grpchealth-go"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"github.com/soheilhy/cmux"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/core"
	packagesv1alpha1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/core/packages/v1alpha1"
	pluginsv1alpha1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/core/plugins/v1alpha1"
	packagesGRPCv1alpha1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/packages/v1alpha1"
	pluginsGRPCv1alpha1 "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/plugins/v1alpha1"
	pluginsConnect "github.com/vmware-tanzu/kubeapps/cmd/kubeapps-apis/gen/core/plugins/v1alpha1/v1alpha1connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	klogv2 "k8s.io/klog/v2"
)

func getLogLevelOfEndpoint(endpoint string) klogv2.Level {

	// Add all endpoint function names which you want to suppress in interceptor logging
	suppressLoggingOfEndpoints := []string{"GetConfiguredPlugins"}
	var level klogv2.Level

	// level=3 is default logging level
	level = 3
	for i := 0; i < len(suppressLoggingOfEndpoints); i++ {
		if strings.Contains(endpoint, suppressLoggingOfEndpoints[i]) {
			level = 4
			break
		}
	}

	return level
}

// LogRequest is a gRPC UnaryServerInterceptor that will log the API call
func LogRequest(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (response interface{}, err error) {

	start := time.Now()
	res, err := handler(ctx, req)

	level := getLogLevelOfEndpoint(info.FullMethod)

	// Format string : [status code] [duration] [full path]
	// OK 97.752µs /kubeappsapis.core.packages.v1alpha1.PackagesService/GetAvailablePackageSummaries
	klogv2.V(level).Infof("%v %s %s\n",
		status.Code(err),
		time.Since(start),
		info.FullMethod)

	return res, err
}

// Serve is the root command that is run when no other sub-commands are present.
// It runs the gRPC service, registering the configured plugins.
func Serve(serveOpts core.ServeOptions) error {
	listenAddr := fmt.Sprintf(":%d", serveOpts.Port)
	// Note: Currently transitioning from the un-maintained improbable-eng grpc library
	// to the connect one. During the transition, some gRPC services are running on the
	// improbable grpc server. Those calls are proxied through, but in a few PRs we'll have
	// all services on the new server and can remove the proxy.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	grpcSrv, gwArgs, listenerCMux, err := createImprobableGRPCServer(ctx, listenAddr)
	if err != nil {
		return fmt.Errorf("failed to create gRPC server: %w", err)
	}

	// The connect service handler automatically handles grpc-web, connect and
	// grpc for us, so we won't need all the extra code below once all services
	// have been transitioned to the new mux (and we can remove the use of cmux
	// once connect is used for all requests).

	// During the transition we use the connect grpc mux by default and any unhandled paths
	// are proxied to the old cmux handler's listener.
	mux_connect := http.NewServeMux()

	// Create the core.plugins.v1alpha1 server which handles registration of
	// plugins, and register it for both grpc and http.
	pluginsServer, err := pluginsv1alpha1.NewPluginsServer(serveOpts, grpcSrv, gwArgs)
	if err != nil {
		return fmt.Errorf("failed to initialize plugins server: %v", err)
	}
	err = registerPluginsServiceServer(mux_connect, pluginsServer, gwArgs)
	if err != nil {
		return fmt.Errorf("failed to register plugins server: %v", err)
	}

	// The gRPC Health checker reports on all connected services.
	checker := grpchealth.NewStaticChecker(
		pluginsConnect.PluginsServiceName,
	)
	mux_connect.Handle(grpchealth.NewHandler(checker))

	port, err := startImprobableHandler(pluginsServer, *listenerCMux, grpcSrv, gwArgs)
	if err != nil {
		return err
	}

	if serveOpts.UnsafeLocalDevKubeconfig {
		klogv2.Warning("Using the local Kubeconfig file instead of the actual in-cluster's config. This is not recommended except for development purposes.")
	}

	// Finally, link the new mux so that all other requests are proxied to the port on which
	// the improbable gRPC server is listening.
	mux_connect.Handle("/", createProxyToImprobableHandler(port))

	klogv2.Infof("Starting server on %q", listenAddr)
	if err := http.ListenAndServe(listenAddr, h2c.NewHandler(mux_connect, &http2.Server{})); err != nil {
		klogv2.Fatalf("failed to server: %+v", err)
	}

	return nil
}

func registerPackagesServiceServer(grpcSrv *grpc.Server, pluginsServer *pluginsv1alpha1.PluginsServer, gwArgs core.GatewayHandlerArgs) error {
	// Ask the plugins server for plugins with GRPC servers that fulfil the core
	// packaging v1alpha1 API, then pass to the constructor below.
	// The argument for the reflect.TypeOf is based on what grpc-go
	// does itself at:
	// https://github.com/grpc/grpc-go/blob/v1.38.0/server.go#L621
	packagingPlugins := pluginsServer.GetPluginsSatisfyingInterface(reflect.TypeOf((*packagesGRPCv1alpha1.PackagesServiceServer)(nil)).Elem())

	// Create the core.packages server and register it for both grpc and http.
	packagesServer, err := packagesv1alpha1.NewPackagesServer(packagingPlugins)
	if err != nil {
		return fmt.Errorf("failed to create core.packages.v1alpha1 server: %w", err)
	}
	packagesGRPCv1alpha1.RegisterPackagesServiceServer(grpcSrv, packagesServer)
	err = packagesGRPCv1alpha1.RegisterPackagesServiceHandlerFromEndpoint(gwArgs.Ctx, gwArgs.Mux, gwArgs.Addr, gwArgs.DialOptions)
	if err != nil {
		return fmt.Errorf("failed to register core.packages handler for gateway: %v", err)
	}
	return nil
}

func registerRepositoriesServiceServer(grpcSrv *grpc.Server, pluginsServer *pluginsv1alpha1.PluginsServer, gwArgs core.GatewayHandlerArgs) error {
	// see comment in registerPackagesServiceServer
	repositoriesPlugins := pluginsServer.GetPluginsSatisfyingInterface(reflect.TypeOf((*packagesGRPCv1alpha1.RepositoriesServiceServer)(nil)).Elem())

	// Create the core.packages server and register it for both grpc and http.
	repoServer, err := packagesv1alpha1.NewRepositoriesServer(repositoriesPlugins)
	if err != nil {
		return fmt.Errorf("failed to create core.packages.v1alpha1 server: %w", err)
	}
	packagesGRPCv1alpha1.RegisterRepositoriesServiceServer(grpcSrv, repoServer)
	err = packagesGRPCv1alpha1.RegisterRepositoriesServiceHandlerFromEndpoint(gwArgs.Ctx, gwArgs.Mux, gwArgs.Addr, gwArgs.DialOptions)
	if err != nil {
		return fmt.Errorf("failed to register core.packages handler for gateway: %v", err)
	}
	return nil
}

// Create a gateway mux that does not emit unpopulated fields.
func gatewayMux() (*runtime.ServeMux, error) {
	gwmux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				EmitUnpopulated: false,
			},
			UnmarshalOptions: protojson.UnmarshalOptions{
				DiscardUnknown: true,
			},
		}),
	)

	// TODO(agamez): remove these '/openapi.json' and '/docs' paths. They are serving a
	// static 'swagger-ui' dashboard with hardcoded values just intended for development purposes.
	// This docs will eventually converge into the docs already (properly) served by the dashboard
	err := gwmux.HandlePath(http.MethodGet, "/openapi.json", runtime.HandlerFunc(func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		http.ServeFile(w, r, "docs/kubeapps-apis.swagger.json")
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to serve: %v", err)
	}

	err = gwmux.HandlePath(http.MethodGet, "/docs", runtime.HandlerFunc(func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		http.ServeFile(w, r, "docs/index.html")
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to serve: %v", err)
	}

	svcRestConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve in cluster configuration: %v", err)
	}
	coreClientSet, err := kubernetes.NewForConfig(svcRestConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve clientset: %v", err)
	}

	// TODO(rcastelblanq) Move this endpoint to the Operators plugin when implementing #4920
	// Proxies the operator icon request to K8s
	err = gwmux.HandlePath(http.MethodGet, "/operators/namespaces/{namespace}/operator/{name}/logo", func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		namespace := pathParams["namespace"]
		name := pathParams["name"]

		logoBytes, err := coreClientSet.RESTClient().Get().AbsPath(fmt.Sprintf("/apis/packages.operators.coreos.com/v1/namespaces/%s/packagemanifests/%s/icon", namespace, name)).Do(context.TODO()).Raw()
		if err != nil {
			http.Error(w, fmt.Sprintf("Unable to retrieve operator logo: %v", err), http.StatusInternalServerError)
			return
		}

		contentType := http.DetectContentType(logoBytes)
		if strings.Contains(contentType, "text/") {
			// DetectContentType is unable to return svg icons since they are in fact text
			contentType = "image/svg+xml"
		}
		w.Header().Set("Content-Type", contentType)
		_, err = w.Write(logoBytes)
		if err != nil {
			return
		}
	})
	if err != nil {
		return nil, fmt.Errorf("failed to serve: %v", err)
	}

	return gwmux, nil
}

// createProxyToImprobableHandler returns a handler func that proxies requests
// through to the improbable handler listening on a different port.
//
// It creates two reverse proxies, one with an h2c transport, the other with an http1 transport,
// so that, depending on the request being handled, the request can be sent on the correct
// transport.
//
// This function is temporary and will be removed once all code is switched to the connect
// gRPC library.
func createProxyToImprobableHandler(port int) http.HandlerFunc {
	h2cProxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)
		},
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
		},
	}
	http1Proxy := httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			h2cProxy.ServeHTTP(w, r)
		} else {
			http1Proxy.ServeHTTP(w, r)
		}
	})
}

// createImprobableGRPCServer returns the created listener as well as the server and gateway arges.
//
// The latter are still required when registering plugins (though will be removed soon).
func createImprobableGRPCServer(ctx context.Context, listenAddr string) (*grpc.Server, core.GatewayHandlerArgs, *net.Listener, error) {
	// Create the grpc server and register the reflection server (for now, useful for discovery
	// using grpcurl) or similar.
	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(LogRequest))
	reflection.Register(grpcSrv)

	gw, err := gatewayMux()
	if err != nil {
		return nil, core.GatewayHandlerArgs{}, nil, err
	}

	// During the transition to the connect gRPC handlers, we'll continue to proxy unhandled
	// gRPC requests through to the old improbable-eng-based handlers which used the cmux
	// library to multiplex requests based on headers. The cmux listen address
	// will be a random port. We'll send traffic through to this port from the main http.mux
	// used by connect.
	listenerCMux, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, core.GatewayHandlerArgs{}, nil, err
	}

	// Note: we point the gateway at our *new* gRPC handler, so that we can continue to use
	// the gateway for a ReST-ish API
	gwArgs := core.GatewayHandlerArgs{
		Ctx:         ctx,
		Mux:         gw,
		Addr:        listenAddr,
		DialOptions: []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	}

	return grpcSrv, gwArgs, &listenerCMux, nil
}

// startImprobableHandler returns the port on which the improbable gRPC handler is listening.
func startImprobableHandler(pluginsServer *pluginsv1alpha1.PluginsServer, listenerCMux net.Listener, grpcSrv *grpc.Server, gwArgs core.GatewayHandlerArgs) (int, error) {

	if err := registerPackagesServiceServer(grpcSrv, pluginsServer, gwArgs); err != nil {
		return 0, err
	} else if err = registerRepositoriesServiceServer(grpcSrv, pluginsServer, gwArgs); err != nil {
		return 0, err
	}

	// Multiplex the connection between grpc and http.
	// Note: due to a change in the grpc protocol, it's no longer possible to just match
	// on the simpler cmux.HTTP2HeaderField("content-type", "application/grpc"). More details
	// at https://github.com/soheilhy/cmux/issues/64
	mux := cmux.New(listenerCMux)
	grpcListener := mux.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	grpcWebListener := mux.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc-web"))
	httpListener := mux.Match(cmux.Any())

	webRpcProxy := grpcweb.WrapServer(grpcSrv,
		grpcweb.WithOriginFunc(func(origin string) bool { return true }),
		grpcweb.WithWebsockets(true),
		grpcweb.WithWebsocketOriginFunc(func(req *http.Request) bool { return true }),
	)

	httpSrv := &http.Server{
		ReadHeaderTimeout: 60 * time.Second, // mitigate slowloris attacks, set to nginx's default
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if webRpcProxy.IsGrpcWebRequest(r) || webRpcProxy.IsAcceptableGrpcCorsRequest(r) || webRpcProxy.IsGrpcWebSocketRequest(r) {
				webRpcProxy.ServeHTTP(w, r)
			} else {
				gwArgs.Mux.ServeHTTP(w, r)
			}
		},
		),
	}

	go func() {
		err := grpcSrv.Serve(grpcListener)
		if err != nil {
			klogv2.Fatalf("failed to serve: %v", err)
		}
	}()
	go func() {
		err := grpcSrv.Serve(grpcWebListener)
		if err != nil {
			klogv2.Fatalf("failed to serve: %v", err)
		}
	}()
	go func() {
		err := httpSrv.Serve(httpListener)
		if err != nil {
			klogv2.Fatalf("failed to serve: %v", err)
		}
	}()
	go func() {
		if err := mux.Serve(); err != nil {
			klogv2.Fatalf("failed to serve: %v", err)
		}
	}()

	parts := strings.SplitAfter(listenerCMux.Addr().String(), ":")
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, err
	}
	return port, nil
}

// Registers the pluginsServer with the mux and gateway.
func registerPluginsServiceServer(mux *http.ServeMux, pluginsServer *pluginsv1alpha1.PluginsServer, gwArgs core.GatewayHandlerArgs) error {
	mux.Handle(pluginsConnect.NewPluginsServiceHandler(pluginsServer))
	err := pluginsGRPCv1alpha1.RegisterPluginsServiceHandlerFromEndpoint(gwArgs.Ctx, gwArgs.Mux, gwArgs.Addr, gwArgs.DialOptions)
	if err != nil {
		return fmt.Errorf("failed to register core.plugins handler for gateway: %v", err)
	}
	return nil
}
