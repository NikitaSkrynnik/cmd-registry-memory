// Copyright (c) 2020-2022 Doc.ai and/or its affiliates.
//
// Copyright (c) 2022 Cisco Systems, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !windows

package main_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/exechelper"
	"github.com/edwarnicke/grpcfd"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/kelseyhightower/envconfig"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/NikitaSkrynnik/api/pkg/api/registry"
	"github.com/NikitaSkrynnik/sdk/pkg/registry/common/begin"
	"github.com/NikitaSkrynnik/sdk/pkg/registry/common/grpcmetadata"
	"github.com/NikitaSkrynnik/sdk/pkg/registry/common/refresh"
	"github.com/NikitaSkrynnik/sdk/pkg/registry/core/next"
	"github.com/NikitaSkrynnik/sdk/pkg/tools/log"
	"github.com/NikitaSkrynnik/sdk/pkg/tools/spiffejwt"
	"github.com/NikitaSkrynnik/sdk/pkg/tools/spire"
	"github.com/NikitaSkrynnik/sdk/pkg/tools/token"

	main "github.com/NikitaSkrynnik/cmd-registry-memory"
)

type RegistryTestSuite struct {
	suite.Suite
	ctx        context.Context
	cancel     context.CancelFunc
	x509source x509svid.Source
	x509bundle x509bundle.Source
	config     main.Config
	spireErrCh <-chan error
	sutErrCh   <-chan error
}

func (t *RegistryTestSuite) SetupSuite() {
	logrus.SetFormatter(&nested.Formatter{})
	log.EnableTracing(true)
	t.ctx, t.cancel = context.WithCancel(context.Background())

	// Run spire
	executable, err := os.Executable()
	require.NoError(t.T(), err)
	t.spireErrCh = spire.Start(
		spire.WithContext(t.ctx),
		spire.WithEntry("spiffe://example.org/registry-memory", "unix:path:/bin/registry-memory"),
		spire.WithEntry(fmt.Sprintf("spiffe://example.org/%s", filepath.Base(executable)),
			fmt.Sprintf("unix:path:%s", executable),
		),
	)
	require.Len(t.T(), t.spireErrCh, 0)

	// Get X509Source
	source, err := workloadapi.NewX509Source(t.ctx)
	t.x509source = source
	t.x509bundle = source
	require.NoError(t.T(), err)
	svid, err := t.x509source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	logrus.Infof("SVID: %q", svid.ID)

	// Run system under test (sut)
	cmdStr := "registry-memory"
	t.sutErrCh = exechelper.Start(cmdStr,
		exechelper.WithContext(t.ctx),
		exechelper.WithEnvirons(os.Environ()...),
		exechelper.WithStdout(os.Stdout),
		exechelper.WithStderr(os.Stderr),
	)
	require.Len(t.T(), t.sutErrCh, 0)

	// Get config from env
	require.NoError(t.T(), envconfig.Process("registry-memory", &t.config))
}

func (t *RegistryTestSuite) TearDownSuite() {
	t.cancel()
	for {
		_, ok := <-t.sutErrCh
		if !ok {
			break
		}
	}
	for {
		_, ok := <-t.spireErrCh
		if !ok {
			break
		}
	}
}

func (t *RegistryTestSuite) TestHealthCheck() {
	ctx, cancel := context.WithTimeout(t.ctx, 100*time.Second)
	defer cancel()
	healthCC, err := grpc.DialContext(ctx,
		t.config.ListenOn[0].String(),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(t.x509source, t.x509bundle, tlsconfig.AuthorizeAny()))),
	)
	if err != nil {
		logrus.Fatalf("Failed healthcheck: %+v", err)
	}
	healthClient := grpc_health_v1.NewHealthClient(healthCC)
	healthResponse, err := healthClient.Check(ctx,
		&grpc_health_v1.HealthCheckRequest{
			Service: "registry.NetworkServiceEndpointRegistry",
		},
		grpc.WaitForReady(true),
	)
	t.NoError(err)
	t.NotNil(healthResponse)
	t.Equal(grpc_health_v1.HealthCheckResponse_SERVING, healthResponse.Status)
	healthResponse, err = healthClient.Check(ctx,
		&grpc_health_v1.HealthCheckRequest{
			Service: "registry.NetworkServiceRegistry",
		},
		grpc.WaitForReady(true),
	)
	t.NoError(err)
	t.NotNil(healthResponse)
	t.Equal(grpc_health_v1.HealthCheckResponse_SERVING, healthResponse.Status)
}

func (t *RegistryTestSuite) TestNetworkServiceRegistration() {
	ctx, cancel := context.WithTimeout(t.ctx, 100*time.Second)
	defer cancel()
	cc, err := grpc.DialContext(ctx,
		t.config.ListenOn[0].String(),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(t.x509source, t.x509bundle, tlsconfig.AuthorizeAny()))),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(t.x509source, t.config.MaxTokenLifetime))),
		),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
	)
	t.NoError(err)
	client := next.NewNetworkServiceRegistryClient(
		grpcmetadata.NewNetworkServiceRegistryClient(),
		registry.NewNetworkServiceRegistryClient(cc),
	)
	_, err = client.Register(context.Background(), &registry.NetworkService{
		Name: "ns-1",
	})
	t.Nil(err)
	stream, err := client.Find(context.Background(), &registry.NetworkServiceQuery{NetworkService: &registry.NetworkService{Name: "ns-1"}})
	t.Nil(err)
	list := registry.ReadNetworkServiceList(stream)
	t.Len(list, 1)
	_, err = client.Unregister(context.Background(), &registry.NetworkService{
		Name: "ns-1",
	})
	t.Nil(err)
	stream, err = client.Find(context.Background(), &registry.NetworkServiceQuery{NetworkService: &registry.NetworkService{Name: "ns-1"}})
	t.Nil(err)
	list = registry.ReadNetworkServiceList(stream)
	t.Len(list, 0)
}

func (t *RegistryTestSuite) TestNetworkServiceEndpointRegistration() {
	ctx, cancel := context.WithTimeout(t.ctx, 100*time.Second)
	defer cancel()
	cc, err := grpc.DialContext(ctx,
		t.config.ListenOn[0].String(),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(t.x509source, t.x509bundle, tlsconfig.AuthorizeAny()))),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(t.x509source, t.config.MaxTokenLifetime))),
		),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
	)
	t.NoError(err)

	client := next.NewNetworkServiceEndpointRegistryClient(
		begin.NewNetworkServiceEndpointRegistryClient(),
		refresh.NewNetworkServiceEndpointRegistryClient(ctx),
		grpcmetadata.NewNetworkServiceEndpointRegistryClient(),
		registry.NewNetworkServiceEndpointRegistryClient(cc),
	)

	result, err := client.Register(context.Background(), &registry.NetworkServiceEndpoint{
		Name: "nse-1",
		Url:  "tcp://127.0.0.1",
		NetworkServiceNames: []string{
			"ns-1",
		},
	})

	t.NoError(err)
	t.NotEmpty(result.Name)
	stream, err := client.Find(context.Background(), &registry.NetworkServiceEndpointQuery{NetworkServiceEndpoint: &registry.NetworkServiceEndpoint{
		Name: result.Name,
	}})
	t.NoError(err)
	list := registry.ReadNetworkServiceEndpointList(stream)
	t.Len(list, 1)
	_, err = client.Unregister(context.Background(), result)
	t.NoError(err)
	stream, err = client.Find(context.Background(), &registry.NetworkServiceEndpointQuery{NetworkServiceEndpoint: result})
	t.NoError(err)
	list = registry.ReadNetworkServiceEndpointList(stream)
	t.Len(list, 0)
}

func (t *RegistryTestSuite) TestNetworkServiceEndpointRegistrationExpiration() {
	ctx, cancel := context.WithTimeout(t.ctx, 100*time.Second)
	defer cancel()
	cc, err := grpc.DialContext(ctx,
		t.config.ListenOn[0].String(),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(t.x509source, t.x509bundle, tlsconfig.AuthorizeAny()))),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(t.x509source, t.config.MaxTokenLifetime))),
		),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
	)
	t.NoError(err)
	client := next.NewNetworkServiceEndpointRegistryClient(
		grpcmetadata.NewNetworkServiceEndpointRegistryClient(),
		registry.NewNetworkServiceEndpointRegistryClient(cc),
	)
	expireTime := time.Now().Add(time.Second)
	result, err := client.Register(context.Background(), &registry.NetworkServiceEndpoint{
		Name: "nse-1",
		Url:  "tcp://127.0.0.1",
		NetworkServiceNames: []string{
			"ns-1",
		},
		ExpirationTime: &timestamp.Timestamp{
			Nanos:   int32(expireTime.Nanosecond()),
			Seconds: expireTime.Unix(),
		},
	})
	t.Nil(err)
	t.NotEmpty(result.Name)
	stream, err := client.Find(context.Background(), &registry.NetworkServiceEndpointQuery{NetworkServiceEndpoint: &registry.NetworkServiceEndpoint{Name: result.Name}})
	t.Nil(err)
	list := registry.ReadNetworkServiceEndpointList(stream)
	t.Len(list, 1)
	t.Eventually(func() bool {
		stream, err = client.Find(context.Background(), &registry.NetworkServiceEndpointQuery{NetworkServiceEndpoint: &registry.NetworkServiceEndpoint{Name: result.Name}})
		t.Nil(err)
		list = registry.ReadNetworkServiceEndpointList(stream)
		return len(list) == 0
	}, time.Until(result.GetExpirationTime().AsTime())+time.Second*5, time.Millisecond*100)
}

func (t *RegistryTestSuite) TestNetworkServiceEndpointClientRefreshingTime() {
	ctx, cancel := context.WithTimeout(t.ctx, 100*time.Second)
	defer cancel()
	cc, err := grpc.DialContext(ctx,
		t.config.ListenOn[0].String(),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsconfig.MTLSClientConfig(t.x509source, t.x509bundle, tlsconfig.AuthorizeAny()))),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(t.x509source, t.config.MaxTokenLifetime))),
		),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
	)
	t.NoError(err)

	clientCount := 10
	var names []string
	for i := 0; i < clientCount; i++ {
		client := next.NewNetworkServiceEndpointRegistryClient(
			begin.NewNetworkServiceEndpointRegistryClient(),
			refresh.NewNetworkServiceEndpointRegistryClient(ctx),
			grpcmetadata.NewNetworkServiceEndpointRegistryClient(),
			registry.NewNetworkServiceEndpointRegistryClient(cc),
		)
		result, regErr := client.Register(context.Background(), &registry.NetworkServiceEndpoint{
			Name: fmt.Sprintf("nse-%d", i),
			Url:  "tcp://127.0.0.1",
			NetworkServiceNames: []string{
				"my-network-service",
			},
		})

		t.NoError(regErr)
		t.NotEmpty(result.Name)
		names = append(names, result.Name)
	}

	client := next.NewNetworkServiceEndpointRegistryClient(
		begin.NewNetworkServiceEndpointRegistryClient(),
		registry.NewNetworkServiceEndpointRegistryClient(cc),
	)

	<-time.After(time.Second)
	stream, err := client.Find(context.Background(), &registry.NetworkServiceEndpointQuery{NetworkServiceEndpoint: &registry.NetworkServiceEndpoint{NetworkServiceNames: []string{
		"my-network-service",
	}}})
	t.Nil(err)
	list := registry.ReadNetworkServiceEndpointList(stream)
	t.Len(list, clientCount)
	for _, name := range names {
		_, err = client.Unregister(ctx, &registry.NetworkServiceEndpoint{Name: name})
	}
	t.NoError(err)
}

func TestRegistryTestSuite(t *testing.T) {
	suite.Run(t, new(RegistryTestSuite))
}
