package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"

	api "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v2"
	xds "github.com/envoyproxy/go-control-plane/pkg/server/v2"

	endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	endpointv2 "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	k8scache "k8s.io/client-go/tools/cache"
)

// Структура EnvoyCluster Имя, Порт,Эндпойнт
type EnvoyCluster struct {
	name      string
	port      uint32
	endpoints []string
}

// Определяем переменные.
var (
	endpoints []types.Resource
	version   int
	// interface SnapshotCache  поддерживающий методы работы со снапшотом. GetSnapshot, SetSnapshot
	snapshotCache cache.SnapshotCache
	// interface SharedIndexInformer поддерживающий методы AddIndexers, GetIndexer
	endpointInformers []k8scache.SharedIndexInformer
)

func main() {
	version = 0

	// initializes a simple cache
	snapshotCache = cache.NewSnapshotCache(false, cache.IDHash{}, nil)

	//Возвращает интерфейс Server is a collection of handlers for streaming discovery requests.
	server := xds.NewServer(context.Background(), snapshotCache, nil)

	// Просто создаем новый GRPC сервер
	grpcServer := grpc.NewServer()
	// Новый листнер на порту 8080
	lis, _ := net.Listen("tcp", ":8080")

	// Регистрируем тип дискавери в нашем GRPC сервере
	discovery.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	// Регистрируем различные API в нашем GRPC сервере.
	api.RegisterEndpointDiscoveryServiceServer(grpcServer, server)
	api.RegisterClusterDiscoveryServiceServer(grpcServer, server)
	api.RegisterRouteDiscoveryServiceServer(grpcServer, server)
	api.RegisterListenerDiscoveryServiceServer(grpcServer, server)

	// Возвращаее в clusters []kubernetes.Interface и ошибку
	clusters, _ := CreateBootstrapClients()

	for _, cluster := range clusters {
		stop := make(chan struct{})
		defer close(stop)

		factory := informers.NewSharedInformerFactoryWithOptions(cluster, time.Second*10, informers.WithNamespace("demo"))
		informer := factory.Core().V1().Endpoints().Informer()
		endpointInformers = append(endpointInformers, informer)

		informer.AddEventHandler(k8scache.ResourceEventHandlerFuncs{
			UpdateFunc: HandleEndpointsUpdate,
		})

		go func() {
			informer.Run(stop)
		}()
	}

	if err := grpcServer.Serve(lis); err != nil {
		fmt.Errorf("", err)
	}
}

func HandleEndpointsUpdate(oldObj, newObj interface{}) {

	edsServiceData := map[string]*EnvoyCluster{}

	for _, inform := range endpointInformers {
		for _, ep := range inform.GetStore().List() {

			endpoints := ep.(*corev1.Endpoints)
			if _, ok := endpoints.Labels["xds"]; !ok {
				continue
			}

			if _, ok := edsServiceData[endpoints.Name]; !ok {
				edsServiceData[endpoints.Name] = &EnvoyCluster{
					name: endpoints.Name,
				}
			}

			for _, subset := range endpoints.Subsets {
				for i, addr := range subset.Addresses {
					edsServiceData[endpoints.Name].port = uint32(subset.Ports[i].Port)
					edsServiceData[endpoints.Name].endpoints = append(edsServiceData[endpoints.Name].endpoints, addr.IP)
				}
			}
		}
	}

	// for each service create endpoints
	edsEndpoints := make([]types.Resource, len(edsServiceData))
	for _, envoyCluster := range edsServiceData {
		edsEndpoints = append(edsEndpoints, MakeEndpointsForCluster(envoyCluster))
	}

	snapshot := cache.NewSnapshot(fmt.Sprintf("%v.0", version), edsEndpoints, nil, nil, nil, nil)

	err := snapshotCache.SetSnapshot("mesh", snapshot)
	if err != nil {
		fmt.Printf("%v", err)
	}

	version++
}

func MakeEndpointsForCluster(service *EnvoyCluster) *endpoint.ClusterLoadAssignment {
	fmt.Printf("Updating endpoints for cluster %s: %v\n", service.name, service.endpoints)
	cla := &endpoint.ClusterLoadAssignment{
		ClusterName: service.name,
		Endpoints:   []*endpointv2.LocalityLbEndpoints{},
	}

	for _, endpoint := range service.endpoints {
		cla.Endpoints = append(cla.Endpoints,
			&endpointv2.LocalityLbEndpoints{
				LbEndpoints: []*endpointv2.LbEndpoint{{
					HostIdentifier: &endpointv2.LbEndpoint_Endpoint{
						Endpoint: &endpointv2.Endpoint{
							Address: &core.Address{
								Address: &core.Address_SocketAddress{
									SocketAddress: &core.SocketAddress{
										Protocol: core.SocketAddress_TCP,
										Address:  endpoint,
										PortSpecifier: &core.SocketAddress_PortValue{
											PortValue: service.port,
										},
									},
								},
							},
						},
					},
				}},
			},
		)
	}
	return cla
}
