/*
Copyright (C) 2022-2023 Traefik Labs

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/

package integration

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/hamba/avro"
	"github.com/hashicorp/yamux"
	"github.com/traefik/hub-agent-kubernetes/pkg/alerting"
	"github.com/traefik/hub-agent-kubernetes/pkg/edgeingress"
	"github.com/traefik/hub-agent-kubernetes/pkg/metrics"
	"github.com/traefik/hub-agent-kubernetes/pkg/metrics/protocol"
	"github.com/traefik/hub-agent-kubernetes/pkg/platform"
	"github.com/traefik/hub-agent-kubernetes/pkg/topology/state"
	"github.com/traefik/hub-agent-kubernetes/pkg/tunnel"
)

const (
	tunnelAcceptBacklog = 256
	minStreamWindowSize = uint32(256 * 1024)
)

type hubHandler struct {
	t               *testing.T
	clusterID       string
	workspaceID     string
	topologyVersion int64
	topology        []byte
	edgeingresses   []edgeingress.EdgeIngress
	brokerListener  net.Listener
	tunnelAddr      string
	caCert          *x509.Certificate
	caPrivKey       *rsa.PrivateKey
	caCertPEM       []byte
}

func (h *hubHandler) Routes() http.Handler {
	router := chi.NewRouter()

	router.Post("/link", h.handleLink)
	router.Post("/ping", h.handlePing)
	router.Get("/config", h.handleConfig)
	router.Get("/wildcard-certificate", h.handleGetWildcardCertificate)
	router.Get("/topology", h.handleFetchTopology)
	router.Patch("/topology", h.handlePatchTopology)

	router.Get("/commands", h.handleListPendingCommands)

	resourcePatterns := "/{resource:(acps|edge-ingresses)}"
	router.Get(resourcePatterns, h.handleListResource)
	router.Post(resourcePatterns, h.handleCreateResource)
	router.Put(resourcePatterns+"/{id}", h.handleUpdateResource)
	router.Delete(resourcePatterns+"/{id}", h.handleDeleteResource)

	router.Get("/data", h.handleGetPreviousData)
	router.Post("/metrics", h.handleSendMetrics)

	router.Get("/rules", h.handleGetRules)
	router.Post("/preflight", h.handlePreflightAlerts)

	router.Get("/tunnel-endpoints", h.handleListClusterTunnelEndpoints)

	router.Get("/fake-tunnel-id", h.handleCreateBrokerTunnel)
	return router
}

func (h *hubHandler) httpError(rw http.ResponseWriter, err error) {
	rw.WriteHeader(http.StatusInternalServerError)
	_, _ = rw.Write([]byte(err.Error()))
}

func (h *hubHandler) createCA() error {
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(2023),
		Subject: pkix.Name{
			Organization: []string{"TraefikLabs"},
			Country:      []string{"FR"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	caPrivKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return err
	}

	caBytes, err := x509.CreateCertificate(rand.Reader, ca, ca, &caPrivKey.PublicKey, caPrivKey)
	if err != nil {
		return err
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caBytes})

	h.caCert = ca
	h.caCertPEM = caPEM
	h.caPrivKey = caPrivKey

	return nil
}

func (h *hubHandler) handleLink(rw http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(rw, `{"clusterID":"%s"}`, h.clusterID)
}

func (h *hubHandler) handlePing(rw http.ResponseWriter, req *http.Request) {
	rw.WriteHeader(http.StatusOK)
}

func (h *hubHandler) handleConfig(rw http.ResponseWriter, req *http.Request) {
	_ = json.NewEncoder(rw).Encode(platform.Config{
		Metrics: platform.MetricsConfig{
			Interval: time.Minute,
			Tables:   []string{"1m", "10m"},
		},
	})
}

func (h *hubHandler) handleGetWildcardCertificate(rw http.ResponseWriter, req *http.Request) {
	certPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	cert := x509.Certificate{
		SerialNumber: bigintPtr(1),
		Subject: pkix.Name{
			Organization: []string{"TraefikLabs"},
		},
		DNSNames: []string{
			"*.localhost",
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(86400 * time.Second),

		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, &cert, h.caCert, certPrivKey.Public(), h.caPrivKey)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(certPrivKey)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
	certPrivKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	err = json.NewEncoder(rw).Encode(edgeingress.Certificate{
		Certificate: certPEM,
		PrivateKey:  certPrivKeyPEM,
	})
	if err != nil {
		h.httpError(rw, err)
		return
	}
}

func (h *hubHandler) handleFetchTopology(rw http.ResponseWriter, req *http.Request) {
	var c state.Cluster
	err := json.Unmarshal(h.topology, &c)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	_ = json.NewEncoder(rw).Encode(struct {
		Version  int64         `json:"version"`
		Topology state.Cluster `json:"topology"`
	}{
		Version:  h.topologyVersion,
		Topology: c,
	})
}

func (h *hubHandler) handlePatchTopology(rw http.ResponseWriter, req *http.Request) {
	body, err := gzip.NewReader(req.Body)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	patch, err := io.ReadAll(body)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	newTopology, err := jsonpatch.MergePatch(h.topology, patch)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	h.topology = newTopology
	h.topologyVersion++

	fmt.Fprintf(rw, `{"version": %d}`, h.topologyVersion)
}

func (h *hubHandler) handleListPendingCommands(rw http.ResponseWriter, req *http.Request) {
	_ = json.NewEncoder(rw).Encode([]platform.Command{})
}

func (h *hubHandler) handleCreateResource(rw http.ResponseWriter, req *http.Request) {
	switch chi.URLParam(req, "resource") {
	case "edge-ingresses":
		var e edgeingress.EdgeIngress
		err := json.NewDecoder(req.Body).Decode(&e)
		if err != nil {
			h.httpError(rw, err)
			return
		}

		e.WorkspaceID = h.workspaceID
		e.ClusterID = h.clusterID
		e.Domain = "test.localhost"
		e.Version = "1"

		rw.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rw).Encode(e)
		h.edgeingresses = append(h.edgeingresses, e)
	default:
		rw.WriteHeader(http.StatusUnprocessableEntity)
	}
}

func (h *hubHandler) handleListResource(rw http.ResponseWriter, req *http.Request) {
	switch chi.URLParam(req, "resource") {
	case "acps":
		_ = json.NewEncoder(rw).Encode([]platform.ACP{})
	case "edge-ingresses":
		_ = json.NewEncoder(rw).Encode(h.edgeingresses)
	default:
		rw.WriteHeader(http.StatusUnprocessableEntity)
	}
}

func (h *hubHandler) handleUpdateResource(rw http.ResponseWriter, req *http.Request) {
	rw.WriteHeader(http.StatusNotImplemented)
}

func (h *hubHandler) handleDeleteResource(rw http.ResponseWriter, req *http.Request) {
	rw.WriteHeader(http.StatusNoContent)
}

func (h *hubHandler) handleGetPreviousData(rw http.ResponseWriter, req *http.Request) {
	schema, err := avro.Parse(protocol.MetricsV2Schema)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	data := map[string][]metrics.DataPointGroup{
		"1m":  {},
		"10m": {},
	}

	err = avro.NewEncoderForSchema(schema, rw).Encode(data)
	if err != nil {
		h.httpError(rw, err)
		return
	}
}

func (h *hubHandler) handleSendMetrics(rw http.ResponseWriter, req *http.Request) {
	rw.WriteHeader(http.StatusOK)
}

func (h *hubHandler) handleGetRules(rw http.ResponseWriter, req *http.Request) {
	_ = json.NewEncoder(rw).Encode([]alerting.Rule{})
}

func (h *hubHandler) handlePreflightAlerts(rw http.ResponseWriter, req *http.Request) {
	_ = json.NewEncoder(rw).Encode([]int{})
}

func (h *hubHandler) handleListClusterTunnelEndpoints(rw http.ResponseWriter, req *http.Request) {
	brokerURL := fmt.Sprintf("ws://%s", h.brokerListener.Addr().String())
	_ = json.NewEncoder(rw).Encode([]tunnel.Endpoint{
		{
			TunnelID:       "fake-tunnel-id",
			BrokerEndpoint: brokerURL,
		},
	})
}

func (h *hubHandler) handleCreateBrokerTunnel(rw http.ResponseWriter, req *http.Request) {
	var upgrader websocket.Upgrader
	websocketConn, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		h.httpError(rw, err)
		return
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("%s:0", getLocalIP()))
	if err != nil {
		h.httpError(rw, err)
		return
	}

	addrParts := strings.Split(listener.Addr().String(), ":")
	h.tunnelAddr = fmt.Sprintf("%s:%s", getLocalIP(), addrParts[len(addrParts)-1])

	outboundConn := &websocketNetConn{Conn: websocketConn}

	session, err := yamux.Server(outboundConn, &yamux.Config{
		EnableKeepAlive:        true,
		LogOutput:              io.Discard,
		AcceptBacklog:          tunnelAcceptBacklog,
		MaxStreamWindowSize:    minStreamWindowSize,
		KeepAliveInterval:      30 * time.Second, // from yamux.DefaultConfig
		ConnectionWriteTimeout: 10 * time.Second, // from yamux.DefaultConfig
		StreamCloseTimeout:     5 * time.Minute,  // from yamux.DefaultConfig
		StreamOpenTimeout:      75 * time.Second, // from yamux.DefaultConfig
	})
	if err != nil {
		h.httpError(rw, err)
		return
	}

	defer func() {
		if sErr := session.Close(); sErr != nil {
			h.t.Error(sErr)
		}
	}()

	listener = &closeAwareListener{Listener: listener}

	listenerClosedCh := make(chan struct{}, 1)
	go func() {
		for {
			inboundConn, aErr := listener.Accept()
			if aErr != nil {
				var netErr net.Error
				if errors.As(aErr, &netErr) && netErr.Temporary() {
					h.t.Log(aErr)
					continue
				}

				if !errors.Is(aErr, errListenerClosed) {
					h.t.Error(aErr)
				}

				break
			}

			go func() {
				stream, sErr := session.Open()
				if sErr != nil {
					h.t.Error(sErr)
					return
				}

				errCh := make(chan error)
				go connCopy(errCh, stream, inboundConn)
				go connCopy(errCh, inboundConn, stream)

				if chErr := <-errCh; err != nil {
					h.t.Error(chErr)
					return
				}
				<-errCh
			}()

			h.t.Log("Not accepting new connection")
		}

		close(listenerClosedCh)
	}()

	for {
		select {
		case <-session.CloseChan():
			h.t.Log("Websocket session closed")
			return
		case <-listenerClosedCh:
			h.t.Log("Listener closed")
			return
		}
	}
}

func getHubRouter(t *testing.T, listener net.Listener) (chi.Router, *hubHandler) {
	t.Helper()

	router := chi.NewRouter()

	if testing.Verbose() {
		router.Use(middleware.Logger)
	}

	hub := hubHandler{t: t, brokerListener: listener, clusterID: "1", workspaceID: "1", topology: []byte("{}")}
	router.Mount("/", hub.Routes())

	return router, &hub
}
