/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/goleak"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/metadata"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	fakeauthenticationv1 "k8s.io/client-go/kubernetes/typed/authentication/v1/fake"
	k8stesting "k8s.io/client-go/testing"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies"
	metricstest "sigs.k8s.io/apiserver-network-proxy/pkg/testing/metrics"
	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

const xfrChannelSize = 10

func TestAgentTokenAuthenticationErrorsToken(t *testing.T) {
	stub := gomock.NewController(t)
	defer stub.Finish()

	ns := "test_ns"
	sa := "test_sa"

	testCases := []struct {
		desc               string
		mdKey              string
		tokens             []string
		wantNamespace      string
		wantServiceAccount string
		authenticated      bool
		authError          string
		tokenReviewError   error
		wantError          bool
	}{
		{
			desc:      "no context",
			wantError: true,
		},
		{
			desc:      "non valid metadata key",
			mdKey:     "someKey",
			tokens:    []string{"token1"},
			wantError: true,
		},
		{
			desc:      "non valid token prefix",
			mdKey:     header.AuthenticationTokenContextKey,
			tokens:    []string{"token1"},
			wantError: true,
		},
		{
			desc:      "multiple valid tokens",
			mdKey:     header.AuthenticationTokenContextKey,
			tokens:    []string{header.AuthenticationTokenContextSchemePrefix + "token1", header.AuthenticationTokenContextSchemePrefix + "token2"},
			wantError: true,
		},
		{
			desc:               "not authenticated",
			authenticated:      false,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			wantNamespace:      ns,
			wantServiceAccount: sa,
			wantError:          true,
		},
		{
			desc:               "tokenReview error",
			authenticated:      false,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			tokenReviewError:   fmt.Errorf("some error"),
			wantNamespace:      ns,
			wantServiceAccount: sa,
			wantError:          true,
		},
		{
			desc:               "non valid namespace",
			authenticated:      true,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			wantNamespace:      "_" + ns,
			wantServiceAccount: sa,
			wantError:          true,
		},
		{
			desc:               "non valid service account",
			authenticated:      true,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			wantNamespace:      ns,
			wantServiceAccount: "_" + sa,
			wantError:          true,
		},
		{
			desc:               "authorization succeed",
			authenticated:      true,
			mdKey:              header.AuthenticationTokenContextKey,
			tokens:             []string{header.AuthenticationTokenContextSchemePrefix + "token1"},
			wantNamespace:      ns,
			wantServiceAccount: sa,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			kcs := k8sfake.NewSimpleClientset()

			kcs.AuthenticationV1().(*fakeauthenticationv1.FakeAuthenticationV1).Fake.PrependReactor("create", "tokenreviews", func(_ k8stesting.Action) (handled bool, ret runtime.Object, err error) {
				tr := &authv1.TokenReview{
					Status: authv1.TokenReviewStatus{
						Authenticated: tc.authenticated,
						Error:         tc.authError,
						User: authv1.UserInfo{
							Username: fmt.Sprintf("system:serviceaccount:%v:%v", ns, sa),
						},
					},
				}
				return true, tr, tc.tokenReviewError
			})

			var md metadata.MD
			for _, token := range tc.tokens {
				md = metadata.Join(md, metadata.Pairs(tc.mdKey, token))
			}

			md = metadata.Join(md, metadata.Pairs(header.AgentID, ""))

			ctx := context.Background()
			defer ctx.Done()
			ctx = metadata.NewIncomingContext(ctx, md)
			conn := agentmock.NewMockAgentService_ConnectServer(stub)
			conn.EXPECT().Context().AnyTimes().Return(ctx)

			// close agent's connection if no error is expected
			if !tc.wantError {
				conn.EXPECT().SendHeader(gomock.Any()).Return(nil)
				conn.EXPECT().Recv().Return(nil, io.EOF)
			}

			p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{
				Enabled:             true,
				KubernetesClient:    kcs,
				AgentNamespace:      tc.wantNamespace,
				AgentServiceAccount: tc.wantServiceAccount,
			}, xfrChannelSize)

			err := p.Connect(conn)
			if tc.wantError {
				if err == nil {
					t.Errorf("test case expected for error")
				}
			} else {
				if err != nil {
					t.Errorf("did not expected for error but got :%v", err)
				}
			}
		})
	}
}

func TestRemovePendingDialForStream(t *testing.T) {
	metrics.Metrics.Reset()
	streamUID := "target-uuid"
	pending1 := &ProxyClientConnection{frontend: &GrpcFrontend{streamUID: streamUID}}
	pending2 := &ProxyClientConnection{}
	pending3 := &ProxyClientConnection{frontend: &GrpcFrontend{streamUID: streamUID}}
	pending4 := &ProxyClientConnection{frontend: &GrpcFrontend{streamUID: "different-uid"}}
	pending5 := &ProxyClientConnection{frontend: &GrpcFrontend{streamUID: ""}}
	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)
	p.PendingDial.Add(1, pending1)
	p.PendingDial.Add(2, pending2)
	p.PendingDial.Add(3, pending3)
	p.PendingDial.Add(4, pending4)
	p.PendingDial.Add(5, pending5)
	p.PendingDial.removeForStream(streamUID)
	expectedPending := map[int64]*ProxyClientConnection{
		int64(2): pending2,
		int64(4): pending4,
		int64(5): pending5,
	}
	if e, a := expectedPending, p.PendingDial.pendingDial; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(3); err != nil {
		t.Errorf("expected pending dial metric to be updated: %v", err)
	}
	p.PendingDial.removeForStream("")
	if e, a := expectedPending, p.PendingDial.pendingDial; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestRemovePendingDialForBackend(t *testing.T) {
	metrics.Metrics.Reset()
	backend1 := &Backend{}
	backend2 := &Backend{}
	pending1 := &ProxyClientConnection{backend: backend1}
	pending2 := &ProxyClientConnection{backend: backend1}
	pending3 := &ProxyClientConnection{backend: backend2}
	pending4 := &ProxyClientConnection{}
	manager := NewPendingDialManager()
	manager.Add(1, pending1)
	manager.Add(2, pending2)
	manager.Add(3, pending3)
	manager.Add(4, pending4)

	removed := manager.removeForBackend(backend1)
	if !reflect.DeepEqual(removed, []*ProxyClientConnection{pending1, pending2}) &&
		!reflect.DeepEqual(removed, []*ProxyClientConnection{pending2, pending1}) {
		t.Fatalf("unexpected removed pending dials: %v", removed)
	}
	expected := map[int64]*ProxyClientConnection{3: pending3, 4: pending4}
	if !reflect.DeepEqual(manager.pendingDial, expected) {
		t.Fatalf("expected pending dials %v, got %v", expected, manager.pendingDial)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(2); err != nil {
		t.Errorf("expected pending dial metric to be updated: %v", err)
	}
	if removed := manager.removeForBackend(nil); len(removed) != 0 {
		t.Fatalf("nil backend removed pending dials: %v", removed)
	}
}

func TestHTTPConnectTunnelBlockedBackendDialSendPreservesBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	metrics.Metrics.Reset()

	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	proxyServer.SetBackendDialTimeout(500 * time.Millisecond)

	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	const unrelatedConnID = int64(900)
	unrelated := &ProxyClientConnection{backend: backend, agentID: backend.GetAgentID(), connectID: unrelatedConnID}
	proxyServer.addEstablished(backend.GetAgentID(), unrelatedConnID, unrelated)
	sendStarted := make(chan *client.Packet, 1)
	releaseSend := make(chan struct{})
	sendReleased := make(chan struct{})
	agentConn.EXPECT().Send(gomock.AssignableToTypeOf(&client.Packet{})).DoAndReturn(func(pkt *client.Packet) error {
		sendStarted <- pkt
		<-releaseSend
		close(sendReleased)
		return nil
	}).Times(1)

	front := httptest.NewServer(&Tunnel{Server: proxyServer})
	defer front.Close()

	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("failed to parse front URL: %v", err)
	}
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatalf("failed to connect to HTTP CONNECT front: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n"); err != nil {
		t.Fatalf("failed to write CONNECT request: %v", err)
	}

	var sent *client.Packet
	select {
	case sent = <-sendStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend DIAL_REQ send to start")
	}
	if sent.Type != client.PacketType_DIAL_REQ {
		t.Fatalf("expected DIAL_REQ to backend, got %v", sent.Type)
	}
	if got := pendingDialCount(proxyServer); got != 1 {
		t.Fatalf("expected one pending dial while backend Send is blocked, got %d", got)
	}

	respCh := make(chan struct {
		resp *http.Response
		err  error
	}, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		respCh <- struct {
			resp *http.Response
			err  error
		}{resp: resp, err: err}
	}()

	var resp *http.Response
	select {
	case got := <-respCh:
		if got.err != nil {
			t.Fatalf("failed to read CONNECT response: %v", got.err)
		}
		resp = got.resp
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for backend dial timeout response")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected %d for blocked backend dial send, got %s", http.StatusGatewayTimeout, resp.Status)
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("expected pending dial to be cleaned after backend dial timeout, got %d", got)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected zero pending dial metric: %v", err)
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailure(metrics.DialFailureBackendDialTimeout, 1); err != nil {
		t.Errorf("expected one backend dial timeout failure: %v", err)
	}
	if got, err := proxyServer.getFrontend(backend.GetAgentID(), unrelatedConnID); err != nil || got != unrelated {
		t.Fatalf("unrelated established connection was removed: got %v, err %v", got, err)
	}
	if backend.IsDraining() {
		t.Fatal("expected backend not to be marked draining after backend dial timeout")
	}
	select {
	case <-backend.Done():
		t.Fatal("expected backend Done channel to remain open after backend dial timeout")
	default:
	}
	for _, bm := range proxyServer.BackendManagers {
		if got := bm.NumBackends(); got != 1 {
			t.Fatalf("expected timed-out backend to remain in manager, got %d backends", got)
		}
	}

	close(releaseSend)
	select {
	case <-sendReleased:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for blocked backend Send goroutine to exit")
	}
	fresh := &client.Packet{Type: client.PacketType_DATA}
	agentConn.EXPECT().Send(fresh).Return(nil).Times(1)
	if err := backend.Send(fresh); err != nil {
		t.Fatalf("healthy backend could not send after dial timeout: %v", err)
	}
	proxyServer.removeEstablished(backend.GetAgentID(), unrelatedConnID)
}

func TestHTTPConnectDialResponseWinsTimeout(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	const dialTimeout = 100 * time.Millisecond
	proxyServer.SetBackendDialTimeout(dialTimeout)
	ownerClaimed := make(chan struct{})
	releaseOwner := make(chan struct{})
	timeoutAttempted := make(chan struct{})
	var releaseOwnerOnce sync.Once
	proxyServer.beforeDialResultPublication = func() {
		close(ownerClaimed)
		<-releaseOwner
	}
	proxyServer.afterHTTPTimeoutDialRemoval = func() { close(timeoutAttempted) }
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	agentPackets := make(chan *client.Packet, 2)
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		agentPackets <- pkt
		return nil
	}).Times(2)

	front := httptest.NewServer(&Tunnel{Server: proxyServer})
	t.Cleanup(func() { closeHTTPTestServer(t, front) })
	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("failed to parse front URL: %v", err)
	}
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatalf("failed to connect to HTTP CONNECT front: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprintf(conn, "CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n"); err != nil {
		t.Fatalf("failed to write CONNECT request: %v", err)
	}

	var dialReq *client.Packet
	select {
	case dialReq = <-agentPackets:
		if dialReq.Type != client.PacketType_DIAL_REQ {
			t.Fatalf("backend packet = %v, want DIAL_REQ", dialReq.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DIAL_REQ")
	}
	const connectID = int64(62)
	recvCh := make(chan *client.Packet, 2)
	backendDone := make(chan struct{})
	go func() {
		proxyServer.serveRecvBackend(backend, backend.GetAgentID(), recvCh)
		close(backendDone)
	}()
	var recvOnce sync.Once
	t.Cleanup(func() {
		recvOnce.Do(func() { close(recvCh) })
		select {
		case <-backendDone:
		case <-time.After(time.Second):
			t.Errorf("timed out joining backend response handler during cleanup")
		}
	})
	// Register this last so a failed handshake guard releases the paused owner
	// before any cleanup waits for the backend or HTTP workers.
	t.Cleanup(func() { releaseOwnerOnce.Do(func() { close(releaseOwner) }) })
	recvCh <- dialRspPkt(dialReq.GetDialRequest().Random, connectID)
	select {
	case <-ownerClaimed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DIAL_RSP owner to remove the pending dial")
	}
	select {
	case <-timeoutAttempted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the real HTTP timeout branch to lose pending ownership")
	}
	releaseOwnerOnce.Do(func() { close(releaseOwner) })
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to bound CONNECT response read: %v", err)
	}
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("failed to read CONNECT response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT response = %s, want 200", resp.Status)
	}
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("failed to bound duplicate response read: %v", err)
	}
	if duplicate, err := http.ReadResponse(reader, nil); err == nil {
		duplicate.Body.Close()
		t.Fatalf("received duplicate terminal HTTP response: %s", duplicate.Status)
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("duplicate response read returned %v, want bounded timeout with active tunnel", err)
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("response winner left %d pending dials", got)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected zero pending dial metric: %v", err)
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailures(map[metrics.DialFailureReason]int{}); err != nil {
		t.Errorf("response winner recorded timeout failure: %v", err)
	}
	proxyServer.fmu.RLock()
	established := proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 1 {
		t.Fatalf("expected one active HTTP connection, got %d", established)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("failed to close HTTP frontend: %v", err)
	}
	select {
	case pkt := <-agentPackets:
		if pkt.Type != client.PacketType_CLOSE_REQ || pkt.GetCloseRequest().ConnectID != connectID {
			t.Fatalf("unexpected tunnel cleanup packet: %v", pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HTTP tunnel cleanup")
	}
	recvCh <- &client.Packet{Type: client.PacketType_CLOSE_RSP, Payload: &client.Packet_CloseResponse{CloseResponse: &client.CloseResponse{ConnectID: connectID}}}
	if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond, time.Second, true, func(context.Context) (bool, error) {
		proxyServer.fmu.RLock()
		defer proxyServer.fmu.RUnlock()
		return proxyServer.getCount(proxyServer.established) == 0, nil
	}); err != nil {
		t.Fatalf("HTTP connection was retained after close response: %v", err)
	}
	recvOnce.Do(func() { close(recvCh) })
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend response handler")
	}
}

func TestHTTPConnectBackendCloseWinsTimeout(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	const dialTimeout = 100 * time.Millisecond
	proxyServer.SetBackendDialTimeout(dialTimeout)
	ownerClaimed := make(chan struct{})
	releaseOwner := make(chan struct{})
	timeoutAttempted := make(chan struct{})
	var releaseOwnerOnce sync.Once
	proxyServer.beforeDialResultPublication = func() {
		close(ownerClaimed)
		<-releaseOwner
	}
	proxyServer.afterHTTPTimeoutDialRemoval = func() { close(timeoutAttempted) }
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	dialSent := make(chan *client.Packet, 1)
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		dialSent <- pkt
		return nil
	}).Times(1)

	front := httptest.NewServer(&Tunnel{Server: proxyServer})
	t.Cleanup(func() { closeHTTPTestServer(t, front) })
	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("failed to parse front URL: %v", err)
	}
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatalf("failed to connect to HTTP CONNECT front: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprintf(conn, "CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n"); err != nil {
		t.Fatalf("failed to write CONNECT request: %v", err)
	}
	select {
	case pkt := <-dialSent:
		if pkt.Type != client.PacketType_DIAL_REQ {
			t.Fatalf("backend packet = %v, want DIAL_REQ", pkt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DIAL_REQ")
	}
	recvCh := make(chan *client.Packet)
	backendDone := make(chan struct{})
	go func() {
		proxyServer.serveRecvBackend(backend, backend.GetAgentID(), recvCh)
		close(backendDone)
	}()
	close(recvCh)
	t.Cleanup(func() {
		select {
		case <-backendDone:
		case <-time.After(time.Second):
			t.Errorf("timed out joining backend-close owner during cleanup")
		}
	})
	// Register this last so a failed handshake guard releases the paused owner
	// before any cleanup waits for the backend or HTTP workers.
	t.Cleanup(func() { releaseOwnerOnce.Do(func() { close(releaseOwner) }) })
	select {
	case <-ownerClaimed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend-close owner to remove the pending dial")
	}
	select {
	case <-timeoutAttempted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the real HTTP timeout branch to lose pending ownership")
	}
	releaseOwnerOnce.Do(func() { close(releaseOwner) })
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend-close owner to publish completion")
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("failed to bound backend-close response read: %v", err)
	}
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("failed to read backend-close response: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("backend-close response = %s, want 502", resp.Status)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("failed to read backend-close response body: %v", err)
	}
	resp.Body.Close()
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("failed to bound duplicate response read: %v", err)
	}
	if _, err := http.ReadResponse(reader, nil); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected exactly one terminal HTTP response, second read error = %v", err)
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("backend close left %d pending dials", got)
	}
	proxyServer.fmu.RLock()
	established := proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 0 {
		t.Fatalf("backend close retained %d established connections", established)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected zero pending dial metric: %v", err)
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailures(map[metrics.DialFailureReason]int{
		metrics.DialFailureBackendClose: 1,
	}); err != nil {
		t.Errorf("expected one backend-close failure and no timeout failure: %v", err)
	}
}

func TestResolveDialSendOnContextDone(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	success := make(chan error, 1)
	success <- nil
	if err := resolveDialSendOnContextDone(expired, success); err != nil {
		t.Fatalf("buffered success returned error: %v", err)
	}

	empty := make(chan error, 1)
	if err := resolveDialSendOnContextDone(expired, empty); !errors.Is(err, errBackendDialTimeout) {
		t.Fatalf("expired context returned %v, want %v", err, errBackendDialTimeout)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := resolveDialSendOnContextDone(canceled, empty); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context returned %v, want context.Canceled", err)
	}
}

func TestConfiguredPendingDialTimeout(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	proxyServer.SetBackendDialTimeout(10 * time.Millisecond)
	_, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	frontendConn := prepareFrontendConn(ctrl)
	frontend := &GrpcFrontend{stream: frontendConn}
	const dialID = int64(71)
	done := make(chan struct{})
	frontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_DIAL_RSP || pkt.GetDialResponse().Random != dialID || pkt.GetDialResponse().Error != errBackendDialTimeout.Error() {
			t.Errorf("unexpected timeout response: %v", pkt)
		}
		close(done)
		return nil
	}).Times(1)
	proxyServer.PendingDial.Add(dialID, &ProxyClientConnection{
		Mode:      ModeGRPC,
		frontend:  frontend,
		dialID:    dialID,
		backend:   backend,
		connected: make(chan struct{}),
	})
	proxyServer.startPendingDialTimeout(dialID, backend, frontend)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pending dial timeout")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("expected no pending dials, got %d", got)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected zero pending dial metric: %v", err)
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailure(metrics.DialFailureBackendDialTimeout, 1); err != nil {
		t.Errorf("expected backend dial timeout failure metric: %v", err)
	}
}

func TestBackendCloseFailsOnlyItsPendingDials(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	_, failedBackend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	_, healthyBackend := prepareAgentConnMD(t, ctrl, proxyServer, nil)

	failedFrontendConn := prepareFrontendConn(ctrl)
	failedFrontend := &GrpcFrontend{stream: failedFrontendConn}
	responses := make(chan int64, 2)
	failedFrontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		resp := pkt.GetDialResponse()
		if pkt.Type != client.PacketType_DIAL_RSP || resp.Error != errBackendClosedWhileDialing.Error() {
			t.Errorf("unexpected backend-close response: %v", pkt)
		}
		responses <- resp.Random
		return nil
	}).Times(2)

	healthyFrontend := &GrpcFrontend{stream: prepareFrontendConn(ctrl)}
	for _, dialID := range []int64{11, 12} {
		proxyServer.PendingDial.Add(dialID, &ProxyClientConnection{
			Mode:        ModeGRPC,
			frontend:    failedFrontend,
			dialID:      dialID,
			backend:     failedBackend,
			connected:   make(chan struct{}),
			dialAddress: "127.0.0.1:443",
		})
	}
	proxyServer.PendingDial.Add(21, &ProxyClientConnection{
		Mode:      ModeGRPC,
		frontend:  healthyFrontend,
		dialID:    21,
		backend:   healthyBackend,
		connected: make(chan struct{}),
	})

	recvCh := make(chan *client.Packet)
	close(recvCh)
	proxyServer.serveRecvBackend(failedBackend, failedBackend.GetAgentID(), recvCh)

	seen := map[int64]bool{}
	for i := 0; i < 2; i++ {
		select {
		case dialID := <-responses:
			seen[dialID] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for backend-close response")
		}
	}
	if !seen[11] || !seen[12] {
		t.Fatalf("missing backend-close responses, got %v", seen)
	}
	if got := pendingDialCount(proxyServer); got != 1 {
		t.Fatalf("expected only healthy backend dial to remain, got %d pending dials", got)
	}
	if got := proxyServer.PendingDial.Remove(21); got == nil || got.backend != healthyBackend {
		t.Fatalf("healthy backend pending dial was not preserved: %v", got)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected pending dial metric to track final map: %v", err)
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailure(metrics.DialFailureBackendClose, 2); err != nil {
		t.Errorf("expected backend-close failures: %v", err)
	}
	for _, manager := range proxyServer.BackendManagers {
		if got := manager.NumBackends(); got != 2 {
			t.Errorf("backend cleanup changed ready backends: got %d, want 2", got)
		}
	}
}

func TestRepeatedBackendCloseChurnPreservesHealthyBackend(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	manager := proxyServer.BackendManagers[0].(*DefaultBackendManager)
	const agentID = "shared-agent"

	healthyConn := mockAgentConn(ctrl, agentID, nil)
	healthyPackets := make(chan *client.Packet, 2)
	healthyRecv := make(chan *client.Packet, 1)
	healthyConn.EXPECT().SendHeader(gomock.Any()).Return(nil).Times(1)
	healthyConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
		pkt, ok := <-healthyRecv
		if !ok {
			return nil, io.EOF
		}
		return pkt, nil
	}).AnyTimes()
	healthyDone := make(chan error, 1)
	go func() { healthyDone <- proxyServer.Connect(healthyConn) }()
	var healthyStopOnce sync.Once
	defer func() { healthyStopOnce.Do(func() { close(healthyRecv) }) }()
	if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond, time.Second, true, func(context.Context) (bool, error) {
		return manager.NumBackends() == 1, nil
	}); err != nil {
		t.Fatalf("healthy backend did not register: %v", err)
	}
	manager.mu.RLock()
	healthyBackend := manager.backends[agentID][0]
	manager.mu.RUnlock()

	const cycles = 8
	for i := 0; i < cycles; i++ {
		failedConn := mockAgentConn(ctrl, agentID, nil)
		failedStop := make(chan struct{})
		var failedStopOnce sync.Once
		defer func() { failedStopOnce.Do(func() { close(failedStop) }) }()
		failedConn.EXPECT().SendHeader(gomock.Any()).Return(nil).Times(1)
		failedConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
			<-failedStop
			return nil, io.EOF
		}).Times(1)
		failedDone := make(chan error, 1)
		go func() { failedDone <- proxyServer.Connect(failedConn) }()
		if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond, time.Second, true, func(context.Context) (bool, error) {
			manager.mu.RLock()
			defer manager.mu.RUnlock()
			return len(manager.backends[agentID]) == 2, nil
		}); err != nil {
			t.Fatalf("cycle %d: failed backend did not register: %v", i, err)
		}
		manager.mu.RLock()
		var failedBackend *Backend
		for _, candidate := range manager.backends[agentID] {
			if candidate != healthyBackend {
				failedBackend = candidate
			}
		}
		manager.mu.RUnlock()
		if failedBackend == nil {
			t.Fatalf("cycle %d: could not identify failed backend", i)
		}

		dialID := int64(1000 + i)
		frontendConn := prepareFrontendConn(ctrl)
		failedResponse := make(chan struct{})
		frontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
			if pkt.Type != client.PacketType_DIAL_RSP || pkt.GetDialResponse().Random != dialID || pkt.GetDialResponse().Error != errBackendClosedWhileDialing.Error() {
				t.Errorf("cycle %d: unexpected backend-close response: %v", i, pkt)
			}
			close(failedResponse)
			return nil
		}).Times(1)
		proxyServer.PendingDial.Add(dialID, &ProxyClientConnection{
			Mode:        ModeGRPC,
			frontend:    &GrpcFrontend{stream: frontendConn},
			dialID:      dialID,
			backend:     failedBackend,
			connected:   make(chan struct{}),
			dialAddress: "127.0.0.1:443",
		})
		failedStopOnce.Do(func() { close(failedStop) })
		select {
		case err := <-failedDone:
			if err != nil {
				t.Fatalf("cycle %d: failed backend Connect returned error: %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("cycle %d: failed backend worker did not join", i)
		}
		select {
		case <-failedResponse:
		case <-time.After(time.Second):
			t.Fatalf("cycle %d: pending dial was not failed", i)
		}
		if got := pendingDialCount(proxyServer); got != 0 {
			t.Fatalf("cycle %d: retained %d pending dials", i, got)
		}
		if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
			t.Errorf("cycle %d: pending gauge did not return to zero: %v", i, err)
		}
		manager.mu.RLock()
		remaining := append([]*Backend(nil), manager.backends[agentID]...)
		manager.mu.RUnlock()
		if len(remaining) != 1 || remaining[0] != healthyBackend {
			t.Fatalf("cycle %d: exact failed backend was not removed; remaining %v", i, remaining)
		}
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailure(metrics.DialFailureBackendClose, cycles); err != nil {
		t.Errorf("expected one backend-close failure per churn cycle: %v", err)
	}

	// Complete a fresh production frontend dial through the surviving stream.
	const dialID = int64(2000)
	const connectID = int64(2001)
	healthyConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		healthyPackets <- pkt
		return nil
	}).Times(2)
	frontendConn := prepareFrontendConn(ctrl)
	allowEOF := make(chan struct{})
	gomock.InOrder(
		frontendConn.EXPECT().Recv().Return(dialReqPkt(dialID), nil),
		frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
			<-allowEOF
			return nil, io.EOF
		}),
	)
	dialSucceeded := make(chan struct{})
	frontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_DIAL_RSP || pkt.GetDialResponse().ConnectID != connectID {
			t.Errorf("unexpected survivor response: %v", pkt)
		}
		close(dialSucceeded)
		return nil
	}).Times(1)
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- proxyServer.Proxy(frontendConn) }()
	select {
	case pkt := <-healthyPackets:
		if pkt.Type != client.PacketType_DIAL_REQ {
			t.Fatalf("surviving backend packet = %v, want DIAL_REQ", pkt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh dial did not use the surviving backend")
	}
	healthyRecv <- dialRspPkt(dialID, connectID)
	select {
	case <-dialSucceeded:
	case <-time.After(time.Second):
		t.Fatal("surviving backend did not complete a fresh dial")
	}
	if ready, _ := proxyServer.Readiness.Ready(); !ready {
		t.Fatal("readiness was not healthy with the surviving backend")
	}
	close(allowEOF)
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatalf("fresh frontend Proxy returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh frontend worker did not join")
	}
	select {
	case pkt := <-healthyPackets:
		if pkt.Type != client.PacketType_CLOSE_REQ || pkt.GetCloseRequest().ConnectID != connectID {
			t.Fatalf("unexpected survivor cleanup packet: %v", pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh connection was not cleaned from the surviving backend")
	}

	healthyStopOnce.Do(func() { close(healthyRecv) })
	select {
	case err := <-healthyDone:
		if err != nil {
			t.Fatalf("healthy backend Connect returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy backend worker did not join")
	}
	if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond, time.Second, true, func(context.Context) (bool, error) {
		return manager.NumBackends() == 0, nil
	}); err != nil {
		t.Fatalf("healthy backend was not removed after shutdown: %v", err)
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("churn test finished with %d pending dials", got)
	}
}

func TestFrontendCloseCleansPendingDialAndMetric(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	frontendConn := prepareFrontendConn(ctrl)
	const dialID = int64(31)
	const connectID = int64(32)
	dialReq := dialReqPkt(dialID)
	gomock.InOrder(
		frontendConn.EXPECT().Recv().Return(dialReq, nil),
		frontendConn.EXPECT().Recv().Return(nil, io.EOF),
	)
	gomock.InOrder(
		agentConn.EXPECT().Send(dialReq).Return(nil),
		agentConn.EXPECT().Send(dialClosePkt(dialID)).Return(nil),
		agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
			if pkt.Type != client.PacketType_CLOSE_REQ || pkt.GetCloseRequest().ConnectID != connectID {
				t.Errorf("unexpected late-response cleanup packet: %v", pkt)
			}
			return nil
		}),
	)

	if err := proxyServer.Proxy(frontendConn); err != nil {
		t.Fatalf("Proxy returned error: %v", err)
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("expected no pending dials, got %d", got)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected zero pending dial metric: %v", err)
	}

	recvCh := make(chan *client.Packet, 1)
	recvCh <- dialRspPkt(dialID, connectID)
	close(recvCh)
	proxyServer.serveRecvBackend(backend, backend.GetAgentID(), recvCh)
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("late response recreated pending state: %d", got)
	}
	proxyServer.fmu.RLock()
	established := proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 0 {
		t.Fatalf("late response recreated established state: %d", established)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("late response changed the zero pending dial metric: %v", err)
	}
	assertEstablishedConnsMetric(t, 0)
	if err := metricstest.DefaultTester.ExpectServerDialFailures(map[metrics.DialFailureReason]int{
		metrics.DialFailureUnrecognizedResponse: 1,
	}); err != nil {
		t.Errorf("expected exactly one unrecognized late response: %v", err)
	}
}

func TestDialResponseRaceWithSendTimeoutPreservesSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	const dialTimeout = 100 * time.Millisecond
	proxyServer.SetBackendDialTimeout(dialTimeout)
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	frontendConn := prepareFrontendConn(ctrl)

	const dialID = int64(41)
	const connectID = int64(42)
	dialReq := dialReqPkt(dialID)
	data := &client.Packet{
		Type: client.PacketType_DATA,
		Payload: &client.Packet_Data{Data: &client.Data{
			ConnectID: connectID,
			Data:      []byte("still-alive"),
		}},
	}
	allowData := make(chan struct{})
	allowEOF := make(chan struct{})
	gomock.InOrder(
		frontendConn.EXPECT().Recv().Return(dialReq, nil),
		frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
			<-allowData
			return data, nil
		}),
		frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
			<-allowEOF
			return nil, io.EOF
		}),
	)

	sendStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	agentPackets := make(chan *client.Packet, 3)
	var releaseOnce sync.Once
	var dataOnce sync.Once
	var eofOnce sync.Once
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		agentPackets <- pkt
		switch pkt.Type {
		case client.PacketType_DIAL_REQ:
			close(sendStarted)
			<-releaseSend
			return nil
		case client.PacketType_DATA, client.PacketType_CLOSE_REQ:
			return nil
		default:
			return fmt.Errorf("unexpected packet sent to backend: %v", pkt.Type)
		}
	}).Times(3)

	responseSent := make(chan struct{})
	frontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		resp := pkt.GetDialResponse()
		if pkt.Type != client.PacketType_DIAL_RSP || resp.Random != dialID || resp.ConnectID != connectID || resp.Error != "" {
			t.Errorf("unexpected dial response: %v", pkt)
		}
		close(responseSent)
		return nil
	}).Times(1)

	backendRecvCh := make(chan *client.Packet, 1)
	backendDone := make(chan struct{})
	var backendCloseOnce sync.Once
	go func() {
		proxyServer.serveRecvBackend(backend, backend.GetAgentID(), backendRecvCh)
		close(backendDone)
	}()
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- proxyServer.Proxy(frontendConn)
	}()
	t.Cleanup(func() {
		dataOnce.Do(func() { close(allowData) })
		eofOnce.Do(func() { close(allowEOF) })
		releaseOnce.Do(func() { close(releaseSend) })
		backendCloseOnce.Do(func() { close(backendRecvCh) })
		select {
		case <-backendDone:
		case <-time.After(time.Second):
		}
	})

	select {
	case <-sendStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DIAL_REQ send")
	}
	backendRecvCh <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{DialResponse: &client.DialResponse{
			Random:    dialID,
			ConnectID: connectID,
		}},
	}
	select {
	case <-responseSent:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for successful DIAL_RSP")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("expected response to own the dial, got %d pending", got)
	}
	proxyServer.fmu.RLock()
	established := proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 1 {
		t.Fatalf("expected exactly one established connection while active, got %d", established)
	}

	// Keep the transport Send blocked beyond the real configured deadline.
	select {
	case <-time.After(dialTimeout + dialTimeout/2):
	case <-proxyDone:
		t.Fatal("frontend ended before the configured dial-send deadline elapsed")
	}
	dataOnce.Do(func() { close(allowData) })
	releaseOnce.Do(func() { close(releaseSend) })

	for _, wantType := range []client.PacketType{client.PacketType_DIAL_REQ, client.PacketType_DATA} {
		select {
		case pkt := <-agentPackets:
			if pkt.Type != wantType {
				t.Fatalf("backend packet type = %v, want %v", pkt.Type, wantType)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for backend packet %v", wantType)
		}
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailures(map[metrics.DialFailureReason]int{}); err != nil {
		t.Errorf("response winner recorded a timeout failure: %v", err)
	}

	eofOnce.Do(func() { close(allowEOF) })
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatalf("Proxy returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Proxy to finish")
	}
	select {
	case pkt := <-agentPackets:
		if pkt.Type != client.PacketType_CLOSE_REQ || pkt.GetCloseRequest().ConnectID != connectID {
			t.Fatalf("unexpected cleanup packet: %v", pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for established connection cleanup")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("expected no pending dials, got %d", got)
	}
	proxyServer.fmu.RLock()
	established = proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 0 {
		t.Fatalf("expected no established connections after frontend close, got %d", established)
	}
	for _, manager := range proxyServer.BackendManagers {
		if got := manager.NumBackends(); got != 1 {
			t.Fatalf("send timeout removed healthy backend: got %d backends", got)
		}
	}
	backendCloseOnce.Do(func() { close(backendRecvCh) })
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend receiver to finish")
	}
}

func TestDialResponseFrontendShutdownDuringDeliveryCleansConnection(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	frontendConn := prepareFrontendConn(ctrl)

	const dialID = int64(45)
	const connectID = int64(46)
	dialReq := dialReqPkt(dialID)
	allowEOF := make(chan struct{})
	gomock.InOrder(
		frontendConn.EXPECT().Recv().Return(dialReq, nil),
		frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
			<-allowEOF
			return nil, io.EOF
		}),
	)

	deliveryStarted := make(chan struct{})
	releaseDelivery := make(chan struct{})
	frontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_DIAL_RSP || pkt.GetDialResponse().ConnectID != connectID {
			t.Errorf("unexpected successful response: %v", pkt)
		}
		close(deliveryStarted)
		<-releaseDelivery
		return nil
	}).Times(1)

	agentPackets := make(chan *client.Packet, 2)
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		agentPackets <- pkt
		return nil
	}).Times(2)

	backendRecvCh := make(chan *client.Packet, 1)
	backendDone := make(chan struct{})
	go func() {
		proxyServer.serveRecvBackend(backend, backend.GetAgentID(), backendRecvCh)
		close(backendDone)
	}()
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- proxyServer.Proxy(frontendConn) }()

	var eofOnce, deliveryOnce, backendOnce sync.Once
	t.Cleanup(func() {
		eofOnce.Do(func() { close(allowEOF) })
		deliveryOnce.Do(func() { close(releaseDelivery) })
		backendOnce.Do(func() { close(backendRecvCh) })
		select {
		case <-backendDone:
		case <-time.After(time.Second):
		}
	})

	select {
	case pkt := <-agentPackets:
		if pkt.Type != client.PacketType_DIAL_REQ {
			t.Fatalf("first backend packet = %v, want DIAL_REQ", pkt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DIAL_REQ")
	}
	backendRecvCh <- dialRspPkt(dialID, connectID)
	select {
	case <-deliveryStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response delivery to pause")
	}
	proxyServer.fmu.RLock()
	established := proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 1 {
		t.Fatalf("response was exposed before established ownership: got %d established", established)
	}

	eofOnce.Do(func() { close(allowEOF) })
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatalf("Proxy returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frontend shutdown")
	}
	select {
	case pkt := <-agentPackets:
		if pkt.Type != client.PacketType_CLOSE_REQ || pkt.GetCloseRequest().ConnectID != connectID {
			t.Fatalf("unexpected backend cleanup packet: %v", pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for agent-side connection cleanup")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("frontend shutdown retained %d pending dials", got)
	}
	proxyServer.fmu.RLock()
	established = proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 0 {
		t.Fatalf("frontend shutdown retained %d established connections", established)
	}

	deliveryOnce.Do(func() { close(releaseDelivery) })
	backendOnce.Do(func() { close(backendRecvCh) })
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend response handler")
	}
}

func TestDialResponseFrontendShutdownDuringPromotionCleansConnection(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	frontendConn := prepareFrontendConn(ctrl)

	const dialID = int64(47)
	const connectID = int64(48)
	allowEOF := make(chan struct{})
	promotionPaused := make(chan struct{})
	releasePromotion := make(chan struct{})
	cleanupContended := make(chan struct{})
	var eofOnce, promotionOnce, backendOnce sync.Once
	proxyServer.afterPendingDialRemoval = func() {
		close(promotionPaused)
		<-releasePromotion
	}
	proxyServer.afterFrontendStreamLockContention = func() { close(cleanupContended) }

	gomock.InOrder(
		frontendConn.EXPECT().Recv().Return(dialReqPkt(dialID), nil),
		frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
			<-allowEOF
			return nil, io.EOF
		}),
	)
	frontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_DIAL_RSP || pkt.GetDialResponse().Random != dialID || pkt.GetDialResponse().ConnectID != connectID {
			t.Errorf("unexpected successful response: %v", pkt)
		}
		return nil
	}).Times(1)

	agentPackets := make(chan *client.Packet, 2)
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		agentPackets <- pkt
		return nil
	}).Times(2)

	backendRecvCh := make(chan *client.Packet, 1)
	backendDone := make(chan struct{})
	go func() {
		proxyServer.serveRecvBackend(backend, backend.GetAgentID(), backendRecvCh)
		close(backendDone)
	}()
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- proxyServer.Proxy(frontendConn) }()
	t.Cleanup(func() {
		eofOnce.Do(func() { close(allowEOF) })
		promotionOnce.Do(func() { close(releasePromotion) })
		backendOnce.Do(func() { close(backendRecvCh) })
		select {
		case <-backendDone:
		case <-time.After(time.Second):
		}
	})

	select {
	case pkt := <-agentPackets:
		if pkt.Type != client.PacketType_DIAL_REQ {
			t.Fatalf("first backend packet = %v, want DIAL_REQ", pkt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DIAL_REQ")
	}
	backendRecvCh <- dialRspPkt(dialID, connectID)
	select {
	case <-promotionPaused:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting in the real pending-removal-to-established-add transition")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("promotion did not remove pending ownership: got %d pending dials", got)
	}
	eofOnce.Do(func() { close(allowEOF) })
	select {
	case <-cleanupContended:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frontend shutdown to contend on ownershipMu")
	}
	select {
	case err := <-proxyDone:
		t.Fatalf("frontend cleanup completed inside the ownership transition: %v", err)
	default:
	}

	promotionOnce.Do(func() { close(releasePromotion) })
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatalf("Proxy returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frontend shutdown after promotion release")
	}
	select {
	case pkt := <-agentPackets:
		if pkt.Type != client.PacketType_CLOSE_REQ || pkt.GetCloseRequest().ConnectID != connectID {
			t.Fatalf("unexpected agent-side cleanup packet: %v", pkt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for agent-side connection cleanup")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("frontend shutdown retained %d pending dials", got)
	}
	proxyServer.fmu.RLock()
	established := proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 0 {
		t.Fatalf("frontend shutdown retained %d established connections", established)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected zero pending dial metric: %v", err)
	}
	assertEstablishedConnsMetric(t, 0)

	backendOnce.Do(func() { close(backendRecvCh) })
	select {
	case <-backendDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend response handler")
	}
}

func TestDialResponseSendErrorRollsBackEstablishedConnection(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	frontendConn := prepareFrontendConn(ctrl)

	const dialID = int64(49)
	const connectID = int64(50)
	frontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_DIAL_RSP || pkt.GetDialResponse().Random != dialID || pkt.GetDialResponse().ConnectID != connectID {
			t.Errorf("unexpected successful response: %v", pkt)
		}
		return errors.New("frontend delivery failed")
	}).Times(1)
	closeSent := make(chan struct{})
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_CLOSE_REQ || pkt.GetCloseRequest().ConnectID != connectID {
			t.Errorf("unexpected rollback packet: %v", pkt)
		}
		close(closeSent)
		return nil
	}).Times(1)
	proxyServer.PendingDial.Add(dialID, &ProxyClientConnection{
		Mode:        ModeGRPC,
		frontend:    &GrpcFrontend{stream: frontendConn, streamUID: "send-error-stream"},
		dialID:      dialID,
		backend:     backend,
		connected:   make(chan struct{}),
		dialAddress: "127.0.0.1:443",
	})

	recvCh := make(chan *client.Packet, 1)
	recvCh <- dialRspPkt(dialID, connectID)
	close(recvCh)
	proxyServer.serveRecvBackend(backend, backend.GetAgentID(), recvCh)
	select {
	case <-closeSent:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backend CLOSE_REQ rollback")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("delivery rollback retained %d pending dials", got)
	}
	proxyServer.fmu.RLock()
	established := proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 0 {
		t.Fatalf("delivery rollback retained %d established connections", established)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected zero pending dial metric: %v", err)
	}
	assertEstablishedConnsMetric(t, 0)
	if err := metricstest.DefaultTester.ExpectServerDialFailures(map[metrics.DialFailureReason]int{
		metrics.DialFailureSendResponse: 1,
	}); err != nil {
		t.Errorf("expected exactly one send-response failure: %v", err)
	}
}

func TestLateDialResponseCleanup(t *testing.T) {
	ctrl := gomock.NewController(t)
	metrics.Metrics.Reset()
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	proxyServer.SetBackendDialTimeout(10 * time.Millisecond)
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)
	const dialID = int64(51)
	const connectID = int64(52)
	frontendConn := prepareFrontendConn(ctrl)
	frontend := &GrpcFrontend{stream: frontendConn}
	timedOut := make(chan struct{})
	frontendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_DIAL_RSP || pkt.GetDialResponse().Random != dialID || pkt.GetDialResponse().Error != errBackendDialTimeout.Error() {
			t.Errorf("unexpected timeout response: %v", pkt)
		}
		close(timedOut)
		return nil
	}).Times(1)
	proxyServer.PendingDial.Add(dialID, &ProxyClientConnection{
		Mode:        ModeGRPC,
		frontend:    frontend,
		dialID:      dialID,
		backend:     backend,
		connected:   make(chan struct{}),
		dialAddress: "127.0.0.1:443",
	})
	proxyServer.startPendingDialTimeout(dialID, backend, frontend)
	select {
	case <-timedOut:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the real pending dial to be removed")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("timeout left %d pending dials", got)
	}
	if err := metricstest.DefaultTester.ExpectServerPendingDials(0); err != nil {
		t.Errorf("expected zero pending dial metric after timeout: %v", err)
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailure(metrics.DialFailureBackendDialTimeout, 1); err != nil {
		t.Errorf("expected one primary timeout failure: %v", err)
	}

	closeSent := make(chan struct{})
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_CLOSE_REQ || pkt.GetCloseRequest().ConnectID != connectID {
			t.Errorf("unexpected late-response cleanup packet: %v", pkt)
		}
		close(closeSent)
		return nil
	}).Times(1)

	recvCh := make(chan *client.Packet, 1)
	recvCh <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{DialResponse: &client.DialResponse{
			Random:    dialID,
			ConnectID: connectID,
		}},
	}
	close(recvCh)
	proxyServer.serveRecvBackend(backend, backend.GetAgentID(), recvCh)
	select {
	case <-closeSent:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for late-response cleanup")
	}
	if got := pendingDialCount(proxyServer); got != 0 {
		t.Fatalf("late response recreated pending state: %d", got)
	}
	proxyServer.fmu.RLock()
	established := proxyServer.getCount(proxyServer.established)
	proxyServer.fmu.RUnlock()
	if established != 0 {
		t.Fatalf("late response recreated established state: %d", established)
	}
	if err := metricstest.DefaultTester.ExpectServerDialFailures(map[metrics.DialFailureReason]int{
		metrics.DialFailureBackendDialTimeout:   1,
		metrics.DialFailureUnrecognizedResponse: 1,
	}); err != nil {
		t.Errorf("expected unrecognized response metric: %v", err)
	}
}

func TestAddRemoveFrontends(t *testing.T) {
	agent1ConnID1 := new(ProxyClientConnection)
	agent1ConnID2 := new(ProxyClientConnection)
	agent2ConnID1 := new(ProxyClientConnection)
	agent2ConnID2 := new(ProxyClientConnection)
	agent3ConnID1 := new(ProxyClientConnection)

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)
	p.addEstablished("agent1", int64(1), agent1ConnID1)
	p.removeEstablished("agent1", int64(1))
	expectedFrontends := make(map[string]map[int64]*ProxyClientConnection)
	if e, a := expectedFrontends, p.established; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)
	p.addEstablished("agent1", int64(1), agent1ConnID1)
	p.addEstablished("agent1", int64(2), agent1ConnID2)
	p.addEstablished("agent2", int64(1), agent2ConnID1)
	p.addEstablished("agent2", int64(2), agent2ConnID2)
	p.addEstablished("agent3", int64(1), agent3ConnID1)
	p.removeEstablished("agent2", int64(1))
	p.removeEstablished("agent2", int64(2))
	p.removeEstablished("agent1", int64(1))
	expectedFrontends = map[string]map[int64]*ProxyClientConnection{
		"agent1": {
			int64(2): agent1ConnID2,
		},
		"agent3": {
			int64(1): agent3ConnID1,
		},
	}
	if e, a := expectedFrontends, p.established; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestAddRemoveBackends_DefaultStrategy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{}))

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)

	p.addBackend(backend1)

	if got, _ := p.getBackend("127.0.0.1"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}

	p.addBackend(backend2)
	p.addBackend(backend3)
	p.removeBackend(backend1)
	p.removeBackend(backend2)

	if got, _ := p.getBackend("127.0.0.1"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}

	p.removeBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestAddRemoveBackends_DefaultRouteStrategy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=false"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"default-route=true"}))

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefaultRoute}, 1, nil, xfrChannelSize)

	p.addBackend(backend1)

	if got, _ := p.getBackend("127.0.0.1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}

	p.addBackend(backend2)

	if got, _ := p.getBackend("127.0.0.1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}

	p.addBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}

	p.removeBackend(backend1)
	p.removeBackend(backend2)

	if got, _ := p.getBackend("127.0.0.1"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}

	p.removeBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestAddRemoveBackends_DestHostStrategy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=true"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"host=node2.mydomain.com&ipv4=5.6.7.8&ipv6=::"}))

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDestHost}, 1, nil, xfrChannelSize)

	p.addBackend(backend1)
	p.addBackend(backend2)
	p.addBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if got, _ := p.getBackend("localhost"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("node1.mydomain.com"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("1.2.3.4"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("9878::7675:1292:9183:7562"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("node2.mydomain.com"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}
	if got, _ := p.getBackend("5.6.7.8"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}
	if got, _ := p.getBackend("::"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}

	p.removeBackend(backend1)
	p.removeBackend(backend2)
	p.removeBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestAddRemoveBackends_DestHostSanitizeRequest(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"host=node2.mydomain.com&ipv4=5.6.7.8&ipv6=::"}))

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDestHost}, 1, nil, xfrChannelSize)

	p.addBackend(backend1)
	p.addBackend(backend2)

	if got, _ := p.getBackend("127.0.0.1:443"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
	if got, _ := p.getBackend("node1.mydomain.com:443"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("node2.mydomain.com:443"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}
}

func TestAddRemoveBackends_DestHostWithDefault(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=false"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"host=node2.mydomain.com&ipv4=5.6.7.8&ipv6=::"}))

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDestHost, proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)

	p.addBackend(backend1)
	p.addBackend(backend2)
	p.addBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got == nil {
		t.Errorf("expected random fallback, got nil")
	}
	if got, _ := p.getBackend("localhost"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("node1.mydomain.com"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("1.2.3.4"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("9878::7675:1292:9183:7562"); got != backend1 {
		t.Errorf("expected %v, got %v", backend1, got)
	}
	if got, _ := p.getBackend("node2.mydomain.com"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}
	if got, _ := p.getBackend("5.6.7.8"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}
	if got, _ := p.getBackend("::"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}

	p.removeBackend(backend1)
	p.removeBackend(backend2)

	if got, _ := p.getBackend("127.0.0.1"); got != backend3 {
		t.Errorf("expected %v, got %v", backend3, got)
	}

	p.removeBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestAddRemoveBackends_DestHostWithDuplicateIdents(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"host=localhost&host=node2.mydomain.com&ipv4=5.6.7.8&ipv6=::"}))

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDestHost, proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)

	p.addBackend(backend1)
	p.addBackend(backend2)
	p.addBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got == nil {
		t.Errorf("expected random fallback, got nil")
	}
	if got, _ := p.getBackend("localhost"); got == nil {
		t.Errorf("expected any backend, got nil")
	}

	p.removeBackend(backend1)
	p.removeBackend(backend3)

	if got, _ := p.getBackend("127.0.0.1"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}
	if got, _ := p.getBackend("localhost"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}
	if got, _ := p.getBackend("node1.mydomain.com"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}
	if got, _ := p.getBackend("1.2.3.4"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}
	if got, _ := p.getBackend("9878::7675:1292:9183:7562"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}
	if got, _ := p.getBackend("node2.mydomain.com"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}
	if got, _ := p.getBackend("5.6.7.8"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}
	if got, _ := p.getBackend("::"); got != backend2 {
		t.Errorf("expected %v, got %v", backend2, got)
	}

	p.removeBackend(backend2)

	if got, _ := p.getBackend("127.0.0.1"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestEstablishedConnsMetric(t *testing.T) {
	metrics.Metrics.Reset()

	agent1ConnID1 := new(ProxyClientConnection)
	agent1ConnID2 := new(ProxyClientConnection)
	agent2ConnID1 := new(ProxyClientConnection)
	agent2ConnID2 := new(ProxyClientConnection)
	agent3ConnID1 := new(ProxyClientConnection)

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)
	p.addEstablished("agent1", int64(1), agent1ConnID1)
	assertEstablishedConnsMetric(t, 1)
	p.addEstablished("agent1", int64(2), agent1ConnID2)
	assertEstablishedConnsMetric(t, 2)
	p.addEstablished("agent2", int64(1), agent2ConnID1)
	assertEstablishedConnsMetric(t, 3)
	p.addEstablished("agent2", int64(2), agent2ConnID2)
	assertEstablishedConnsMetric(t, 4)
	p.addEstablished("agent3", int64(1), agent3ConnID1)
	assertEstablishedConnsMetric(t, 5)
	p.removeEstablished("agent2", int64(1))
	assertEstablishedConnsMetric(t, 4)
	p.removeEstablished("agent2", int64(2))
	assertEstablishedConnsMetric(t, 3)
	p.removeEstablished("agent1", int64(1))
	assertEstablishedConnsMetric(t, 2)
	p.removeEstablished("agent1", int64(2))
	assertEstablishedConnsMetric(t, 1)
	p.removeEstablished("agent3", int64(1))
	assertEstablishedConnsMetric(t, 0)
}

func TestConnectionDurationMetric(t *testing.T) {
	metrics.Metrics.Reset()

	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)
	p.addEstablished("agent1", int64(1), new(ProxyClientConnection))
	p.removeEstablished("agent1", int64(1))

	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	for _, metricFamily := range metricFamilies {
		if metricFamily.GetName() != "konnectivity_network_proxy_server_connection_duration_seconds" {
			continue
		}
		if got := metricFamily.GetMetric()[0].GetHistogram().GetSampleCount(); got != 1 {
			t.Fatalf("expected 1 connection duration observation, got %d", got)
		}
		return
	}
	t.Fatal("connection duration metric not found")
}

func TestRemoveEstablishedForBackendConn(t *testing.T) {
	backend1 := &Backend{}
	backend2 := &Backend{}
	backend3 := &Backend{}
	agent1ConnID1 := &ProxyClientConnection{backend: backend1}
	agent1ConnID2 := &ProxyClientConnection{backend: backend1}
	agent2ConnID1 := &ProxyClientConnection{backend: backend2}
	agent2ConnID2 := &ProxyClientConnection{backend: backend2}
	agent3ConnID1 := &ProxyClientConnection{backend: backend3}
	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)
	p.addEstablished("agent1", int64(1), agent1ConnID1)
	p.addEstablished("agent1", int64(2), agent1ConnID2)
	p.addEstablished("agent2", int64(1), agent2ConnID1)
	p.addEstablished("agent2", int64(2), agent2ConnID2)
	p.addEstablished("agent3", int64(1), agent3ConnID1)
	p.removeEstablishedForBackendConn("agent2", backend2)
	expectedFrontends := map[string]map[int64]*ProxyClientConnection{
		"agent1": {
			int64(1): agent1ConnID1,
			int64(2): agent1ConnID2,
		},
		"agent3": {
			int64(1): agent3ConnID1,
		},
	}
	if e, a := expectedFrontends, p.established; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestRemoveEstablishedForStream(t *testing.T) {
	streamUID := "target-uuid"
	backend1 := &Backend{}
	backend2 := &Backend{}
	backend3 := &Backend{}
	agent1ConnID1 := &ProxyClientConnection{backend: backend1, frontend: &GrpcFrontend{streamUID: streamUID}}
	agent1ConnID2 := &ProxyClientConnection{backend: backend1}
	agent2ConnID1 := &ProxyClientConnection{backend: backend2, frontend: &GrpcFrontend{streamUID: streamUID}}
	agent2ConnID2 := &ProxyClientConnection{backend: backend2}
	agent3ConnID1 := &ProxyClientConnection{backend: backend3, frontend: &GrpcFrontend{streamUID: streamUID}}
	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)
	p.addEstablished("agent1", int64(1), agent1ConnID1)
	p.addEstablished("agent1", int64(2), agent1ConnID2)
	p.addEstablished("agent2", int64(1), agent2ConnID1)
	p.addEstablished("agent2", int64(2), agent2ConnID2)
	p.addEstablished("agent3", int64(1), agent3ConnID1)
	p.removeEstablishedForStream(streamUID)
	expectedFrontends := map[string]map[int64]*ProxyClientConnection{
		"agent1": {
			int64(2): agent1ConnID2,
		},
		"agent2": {
			int64(2): agent2ConnID2,
		},
	}
	if e, a := expectedFrontends, p.established; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func prepareFrontendConn(ctrl *gomock.Controller) *agentmock.MockAgentService_ConnectServer {
	// prepare the connection to fontend  of proxy-server
	// TODO: replace with a mock ProxyService_ProxyServer
	frontendConn := agentmock.NewMockAgentService_ConnectServer(ctrl)
	frontendConnMD := metadata.MD{
		":authority":   []string{"127.0.0.1:8090"},
		"content-type": []string{"application/grpc"},
		"user-agent":   []string{"grpc-go/1.42.0"},
	}
	frontendConnCtx := metadata.NewIncomingContext(context.Background(), frontendConnMD)
	frontendConn.EXPECT().Context().Return(frontendConnCtx).AnyTimes()
	return frontendConn
}

func prepareAgentConnMD(t testing.TB, ctrl *gomock.Controller, proxyServer *ProxyServer, agentidentifiers []string) (*agentmock.MockAgentService_ConnectServer, *Backend) {
	t.Helper()
	if agentidentifiers == nil {
		agentidentifiers = []string{}
	}
	// prepare the the connection to agent of proxy-server
	agentConn := agentmock.NewMockAgentService_ConnectServer(ctrl)
	agentConnMD := metadata.MD{
		":authority":       []string{"127.0.0.1:8091"},
		"agentid":          []string{uuid.New().String()},
		"agentidentifiers": agentidentifiers,
		"content-type":     []string{"application/grpc"},
		"user-agent":       []string{"grpc-go/1.42.0"},
	}
	agentConnCtx := metadata.NewIncomingContext(context.Background(), agentConnMD)
	agentConn.EXPECT().Context().Return(agentConnCtx).AnyTimes()
	backend, err := NewBackend(agentConn)
	if err != nil {
		t.Fatalf("Unexpected NewBackend error: %v", err)
	}
	proxyServer.addBackend(backend)
	return agentConn, backend
}

func baseServerProxyTestWithoutBackend(t *testing.T, validate func(*agentmock.MockAgentService_ConnectServer)) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	frontendConn := prepareFrontendConn(ctrl)
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)

	validate(frontendConn)

	proxyServer.Proxy(frontendConn)
}

func baseServerProxyTestWithBackend(t *testing.T, validate func(*agentmock.MockAgentService_ConnectServer, *agentmock.MockAgentService_ConnectServer)) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	frontendConn := prepareFrontendConn(ctrl)

	// prepare proxy server
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)

	agentConn, _ := prepareAgentConnMD(t, ctrl, proxyServer, nil)

	validate(frontendConn, agentConn)

	proxyServer.Proxy(frontendConn)
}

func TestServerProxyNoBackend(t *testing.T) {
	validate := func(frontendConn *agentmock.MockAgentService_ConnectServer) {
		// receive DIAL_REQ from frontend and proxy to backend
		dialReq := &client.Packet{
			Type: client.PacketType_DIAL_REQ,
			Payload: &client.Packet_DialRequest{
				DialRequest: &client.DialRequest{
					Protocol: "tcp",
					Address:  "127.0.0.1:8080",
					Random:   111,
				},
			},
		}

		dialResp := &client.Packet{
			Type: client.PacketType_DIAL_RSP,
			Payload: &client.Packet_DialResponse{
				DialResponse: &client.DialResponse{
					Random: 111,
					Error:  (&ErrNotFound{}).Error(),
				}},
		}

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(dialReq, nil).Times(1),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF).Times(1),
			// NOTE(mainred): `Send` should come before `Recv` io.EOF, but we cannot add wait between
			//                two Recvs, thus `Recv`` comes before `Send`
			frontendConn.EXPECT().Send(dialResp).Return(nil).Times(1),
		)

	}
	baseServerProxyTestWithoutBackend(t, validate)

	if err := metricstest.DefaultTester.ExpectServerDialFailure(metrics.DialFailureNoAgent, 1); err != nil {
		t.Error(err)
	}
}

func TestServerProxyNormalClose(t *testing.T) {
	validate := func(frontendConn, agentConn *agentmock.MockAgentService_ConnectServer) {
		const dialID = 111
		const connectID = 123456
		// receive DIAL_REQ from frontend and proxy to backend
		dialReq := dialReqPkt(dialID)
		data := dataPkt(connectID, []byte("hello world"))
		closeReq := closeReqPkt(connectID)

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(dialReq, nil).Times(1),
			frontendConn.EXPECT().Recv().Return(data, nil).Times(1),
			frontendConn.EXPECT().Recv().Return(closeReq, nil).Times(1),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF).Times(1),
		)
		gomock.InOrder(
			agentConn.EXPECT().Send(dialReq).Return(nil).Times(1),
			agentConn.EXPECT().Send(data).Return(nil).Times(1),
			agentConn.EXPECT().Send(closeReq).Return(nil).Times(1),
			agentConn.EXPECT().Send(dialClosePkt(dialID)).Return(nil).Times(1),
		)
	}
	baseServerProxyTestWithBackend(t, validate)
}

func TestServerProxyRecvChanFull(t *testing.T) {
	validate := func(frontendConn, agentConn *agentmock.MockAgentService_ConnectServer) {
		const dialID = 111
		const connectID = 1
		// receive DIAL_REQ from frontend and proxy to backend
		dialReq := dialReqPkt(dialID)
		data := dataPkt(connectID, []byte("hello world"))

		const defaultTimeout = 5 * time.Minute
		deadline := time.Now().Add(defaultTimeout)
		if testDeadline, ok := t.Deadline(); ok && testDeadline.Before(deadline) {
			deadline = testDeadline.Add(-1 * time.Second)
		}

		waitForMetricVal := func(expected float64) {
			err := wait.Poll(10*time.Millisecond, time.Until(deadline), func() (bool, error) {
				val := promtest.ToFloat64(metrics.Metrics.FullRecvChannel(metrics.Proxy))
				return val == expected, nil
			})
			if err != nil {
				t.Fatalf("Failed to observe expected metric: %v", err)
			}
		}

		expectMetricVal := func(expected float64) {
			val := promtest.ToFloat64(metrics.Metrics.FullRecvChannel(metrics.Proxy))
			if val != expected {
				t.Errorf("Unexpected metric value: %v (expected %v)", val, expected)
			}
		}

		// WaitGroups for coordinating test stages.
		recvWG := sync.WaitGroup{}
		recvWG.Add(1)
		sendWG := sync.WaitGroup{}
		sendWG.Add(1)

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(dialReq, nil),
			// First packet goes through to agentConn.Send
			frontendConn.EXPECT().Recv().Return(data, nil),
			// Next xfrChannelSize packets fill the channel.
			frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
				// Wait for initial packet send before filling the channel.
				recvWG.Wait()
				return data, nil
			}),
			frontendConn.EXPECT().Recv().Return(data, nil).Times(xfrChannelSize-1),
			// Last packet should trigger channel full condition.
			frontendConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
				// Verify that the full channel condition hasn't triggered yet.
				expectMetricVal(0)
				return data, nil
			}),

			frontendConn.EXPECT().Recv().Return(closeReqPkt(1), nil),
			// Ensure that the go-routines don't deadlock if more packets are received before closing the connection.
			// This is a bit contrived, but exercises a possible failure scenario.
			frontendConn.EXPECT().Recv().Return(data, nil).Times(xfrChannelSize+1),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF),
		)
		gomock.InOrder(
			agentConn.EXPECT().Send(dialReq).Return(nil),
			agentConn.EXPECT().Send(data).DoAndReturn(func(_ *client.Packet) error {
				// Channel should not be full at this point.
				expectMetricVal(0)
				recvWG.Done() // Proceed to fill the channel.

				// Block the send from completing until the full channel condition is detected.
				waitForMetricVal(1)
				return nil
			}),
			agentConn.EXPECT().Send(data).Return(nil).Times(xfrChannelSize+1), // Expect the remaining packets to be sent.
			agentConn.EXPECT().Send(closeReqPkt(1)).Return(nil),
			agentConn.EXPECT().Send(dialClosePkt(dialID)).Return(nil).Times(1),
		)
	}
	baseServerProxyTestWithBackend(t, validate)
}

func TestServerProxyNoDial(t *testing.T) {
	baseServerProxyTestWithBackend(t, func(frontendConn, _ *agentmock.MockAgentService_ConnectServer) {
		const connectID = 123456
		data := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: connectID,
				},
			},
		}

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(data, nil),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF),
		)
		frontendConn.EXPECT().Send(closeRspPkt(connectID, "backend not initialized")).Return(nil)
	})
}

func TestServerProxyConnectionMismatch(t *testing.T) {
	baseServerProxyTestWithBackend(t, func(frontendConn, agentConn *agentmock.MockAgentService_ConnectServer) {
		const dialID = 111
		const firstConnectID = 123456
		const secondConnectID = 654321
		dialReq := dialReqPkt(dialID)
		data := dataPkt(firstConnectID, []byte("hello"))
		mismatchedData := dataPkt(secondConnectID, []byte("world"))

		gomock.InOrder(
			frontendConn.EXPECT().Recv().Return(dialReq, nil),
			frontendConn.EXPECT().Recv().Return(data, nil),
			frontendConn.EXPECT().Recv().Return(mismatchedData, nil),
			frontendConn.EXPECT().Recv().Return(nil, io.EOF),
		)
		gomock.InOrder(
			agentConn.EXPECT().Send(dialReq).Return(nil),
			agentConn.EXPECT().Send(data).Return(nil),
		)
		agentConn.EXPECT().Send(closeReqPkt(secondConnectID)).Return(nil)
		agentConn.EXPECT().Send(closeReqPkt(firstConnectID)).Return(nil)
		frontendConn.EXPECT().Send(closeRspPkt(secondConnectID, "mismatched connection IDs")).Return(nil)
		frontendConn.EXPECT().Send(closeRspPkt(firstConnectID, "mismatched connection IDs")).Return(nil)
		agentConn.EXPECT().Send(dialClosePkt(dialID)).Return(nil).Times(1)
	})
}

func TestReadyBackendsMetric(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	metrics.Metrics.Reset()

	p := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	assertReadyBackendsMetric(t, 0)

	_, backend := prepareAgentConnMD(t, ctrl, p, nil)
	assertReadyBackendsMetric(t, 1)

	p.removeBackend(backend)
	assertReadyBackendsMetric(t, 0)
}

func TestTotalReadyBackendsMetric(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	metrics.Metrics.Reset()

	p := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault, proxystrategies.ProxyStrategyDestHost}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	assertTotalReadyBackendsMetric(t, map[string]int{proxystrategies.ProxyStrategyDefault.String(): 0, proxystrategies.ProxyStrategyDestHost.String(): 0})

	_, backend1 := prepareAgentConnMD(t, ctrl, p, nil)
	assertTotalReadyBackendsMetric(t, map[string]int{proxystrategies.ProxyStrategyDefault.String(): 1, proxystrategies.ProxyStrategyDestHost.String(): 0})

	// Add a backend with IPv4 agent identifier.
	_, backend2 := prepareAgentConnMD(t, ctrl, p, []string{"host=localhost"})
	assertTotalReadyBackendsMetric(t, map[string]int{proxystrategies.ProxyStrategyDefault.String(): 2, proxystrategies.ProxyStrategyDestHost.String(): 1})

	p.removeBackend(backend1)
	p.removeBackend(backend2)
	assertTotalReadyBackendsMetric(t, map[string]int{proxystrategies.ProxyStrategyDefault.String(): 0, proxystrategies.ProxyStrategyDestHost.String(): 0})
}

func dialReqPkt(dialID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DIAL_REQ,
		Payload: &client.Packet_DialRequest{
			DialRequest: &client.DialRequest{
				Protocol: "tcp",
				Address:  "127.0.0.1:8080",
				Random:   dialID,
			},
		},
	}
}

func closeHTTPTestServer(t testing.TB, server *httptest.Server) {
	t.Helper()
	server.CloseClientConnections()
	done := make(chan struct{})
	go func() {
		server.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("timed out joining HTTP test server during cleanup")
	}
}

func dialRspPkt(dialID, connectID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{DialResponse: &client.DialResponse{
			Random:    dialID,
			ConnectID: connectID,
		}},
	}
}

func dataPkt(connectID int64, data []byte) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DATA,
		Payload: &client.Packet_Data{
			Data: &client.Data{
				ConnectID: connectID,
				Data:      data,
			},
		},
	}
}

func closeReqPkt(connectID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_CLOSE_REQ,
		Payload: &client.Packet_CloseRequest{
			CloseRequest: &client.CloseRequest{
				ConnectID: connectID,
			}},
	}
}

func closeRspPkt(connectID int64, errMsg string) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{
				ConnectID: connectID,
				Error:     errMsg,
			},
		},
	}
}

func assertEstablishedConnsMetric(t testing.TB, expect int) {
	t.Helper()
	if err := metricstest.DefaultTester.ExpectServerEstablishedConns(expect); err != nil {
		t.Errorf("Expected %d %s metric: %v", expect, "established_connections", err)
	}
}

func pendingDialCount(p *ProxyServer) int {
	p.PendingDial.mu.RLock()
	defer p.PendingDial.mu.RUnlock()
	return len(p.PendingDial.pendingDial)
}

func assertReadyBackendsMetric(t testing.TB, expect int) {
	t.Helper()
	if err := metricstest.DefaultTester.ExpectServerReadyBackends(expect); err != nil {
		t.Errorf("Expected %d %s metric: %v", expect, "ready_backend_connections", err)
	}
}

func assertTotalReadyBackendsMetric(t testing.TB, expect map[string]int) {
	t.Helper()
	if err := metricstest.DefaultTester.ExpectServerTotalReadyBackends(expect); err != nil {
		t.Errorf("Expected %s metric for each proxy strategy %+v, but got error: %v", "ready_backends", expect, err)
	}
}

func dialClosePkt(dialID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DIAL_CLS,
		Payload: &client.Packet_CloseDial{
			CloseDial: &client.CloseDial{
				Random: dialID,
			},
		},
	}
}

func TestRemoveEstablishedForBackendConnPreservesOtherBackends(t *testing.T) {
	p := NewProxyServer("", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, xfrChannelSize)

	backend1 := &Backend{}
	backend2 := &Backend{}

	conn1 := &ProxyClientConnection{backend: backend1}
	conn2 := &ProxyClientConnection{backend: backend2}
	p.addEstablished("agent1", int64(1), conn1)
	p.addEstablished("agent1", int64(2), conn2)

	ret, err := p.removeEstablishedForBackendConn("agent1", backend1)
	if err != nil {
		t.Fatalf("removeEstablishedForBackendConn returned error: %v", err)
	}
	if len(ret) != 1 || ret[0] != conn1 {
		t.Fatalf("expected only conn1 returned, got %v", ret)
	}
	if got, err := p.getFrontend("agent1", int64(2)); err != nil || got != conn2 {
		t.Errorf("conn2 on backend2 was wrongly evicted: got %v, err %v", got, err)
	}
	if got, err := p.getFrontend("agent1", int64(1)); err == nil && got != nil {
		t.Errorf("conn1 on backend1 should have been removed, got %v", got)
	}
}
