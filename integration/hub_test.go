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
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/hamba/avro"
	"github.com/traefik/hub-agent-kubernetes/pkg/alerting"
	"github.com/traefik/hub-agent-kubernetes/pkg/edgeingress"
	"github.com/traefik/hub-agent-kubernetes/pkg/metrics"
	"github.com/traefik/hub-agent-kubernetes/pkg/metrics/protocol"
	"github.com/traefik/hub-agent-kubernetes/pkg/platform"
	"github.com/traefik/hub-agent-kubernetes/pkg/topology/state"
	"github.com/traefik/hub-agent-kubernetes/pkg/tunnel"
)

type hubHandler struct {
	clusterID       string
	workspaceID     string
	topologyVersion int64
	topology        []byte
	edgeingresses   []edgeingress.EdgeIngress
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

	router.Get("/rules", h.handleGetRules)
	router.Post("/preflight", h.handlePreflightAlerts)

	router.Get("/tunnel-endpoints", h.handleListClusterTunnelEndpoints)

	router.Get("/test-ingress", h.handleTestIngress)
	return router
}

func (h *hubHandler) handleLink(rw http.ResponseWriter, req *http.Request) {
	_, _ = rw.Write([]byte(`{"clusterID":"1"}`))
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
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}

	template := x509.Certificate{
		SerialNumber: bigintPtr(1),
		Subject: pkix.Name{
			Organization: []string{"Acme Co"},
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

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, priv.Public(), priv)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}

	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})

	err = json.NewEncoder(rw).Encode(edgeingress.Certificate{
		Certificate: cert,
		PrivateKey:  key,
	})
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}
}

func (h *hubHandler) handleFetchTopology(rw http.ResponseWriter, req *http.Request) {
	var c state.Cluster
	err := json.Unmarshal(h.topology, &c)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
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
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}

	patch, err := io.ReadAll(body)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}

	newTopology, err := jsonpatch.MergePatch(h.topology, patch)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
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
			rw.WriteHeader(http.StatusInternalServerError)
			_, _ = rw.Write([]byte(err.Error()))
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
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}

	data := map[string][]metrics.DataPointGroup{
		"1m":  {},
		"10m": {},
	}

	err = avro.NewEncoderForSchema(schema, rw).Encode(data)
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte(err.Error()))
		return
	}
}

func (h *hubHandler) handleGetRules(rw http.ResponseWriter, req *http.Request) {
	_ = json.NewEncoder(rw).Encode([]alerting.Rule{})
}

func (h *hubHandler) handlePreflightAlerts(rw http.ResponseWriter, req *http.Request) {
	_ = json.NewEncoder(rw).Encode([]int{})
}

func (h *hubHandler) handleListClusterTunnelEndpoints(rw http.ResponseWriter, req *http.Request) {
	_ = json.NewEncoder(rw).Encode([]tunnel.Endpoint{})
}

func (h *hubHandler) handleTestIngress(rw http.ResponseWriter, req *http.Request) {
	_, _ = rw.Write([]byte(`It works !`))
}

func getHubRouter() chi.Router {
	router := chi.NewRouter()

	if testing.Verbose() {
		router.Use(middleware.Logger)
	}

	hub := hubHandler{clusterID: "1", workspaceID: "1", topology: []byte("{}")}
	router.Mount("/", hub.Routes())

	return router
}
