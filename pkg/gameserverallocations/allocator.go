// Copyright 2019 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gameserverallocations

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	multiclusterv1alpha1 "agones.dev/agones/pkg/apis/multicluster/v1alpha1"
	multiclusterinformerv1alpha1 "agones.dev/agones/pkg/client/informers/externalversions/multicluster/v1alpha1"
	multiclusterlisterv1alpha1 "agones.dev/agones/pkg/client/listers/multicluster/v1alpha1"
	"agones.dev/agones/pkg/util/apiserver"
	"agones.dev/agones/pkg/util/logfields"
	"agones.dev/agones/pkg/util/runtime"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1lister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

var (
	// ErrNoGameServerReady is returned when there are no Ready GameServers
	// available
	ErrNoGameServerReady = errors.New("Could not find a Ready GameServer")
	// ErrConflictInGameServerSelection is returned when the candidate gameserver already allocated
	ErrConflictInGameServerSelection = errors.New("The Gameserver was already allocated")
)

const (
	secretClientCertName = "tls.crt"
	secretClientKeyName  = "tls.key"
	secretCaCertName     = "ca.crt"

	// Instead of selecting the top one, controller selects a random one
	// from the topNGameServerCount of Ready gameservers
	// to reduce the contention while allocating gameservers.
	topNGameServerDefaultCount = 100
)

const (
	maxBatchQueue         = 100
	maxBatchBeforeRefresh = 100
	batchWaitTime         = 500 * time.Millisecond
)

var allocationRetry = wait.Backoff{
	Steps:    5,
	Duration: 10 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

// Allocator handles game server allocation
type Allocator struct {
	baseLogger             *logrus.Entry
	allocationPolicyLister multiclusterlisterv1alpha1.GameServerAllocationPolicyLister
	allocationPolicySynced cache.InformerSynced
	secretLister           corev1lister.SecretLister
	secretSynced           cache.InformerSynced
	recorder               record.EventRecorder
	pendingRequests        chan request
	readyGameServerCache   *ReadyGameServerCache
	topNGameServerCount    int
}

// request is an async request for allocation
type request struct {
	gsa      *allocationv1.GameServerAllocation
	response chan response
}

// response is an async response for a matching request
type response struct {
	request request
	gs      *agonesv1.GameServer
	err     error
}

// NewAllocator creates an instance off Allocator
func NewAllocator(policyInformer multiclusterinformerv1alpha1.GameServerAllocationPolicyInformer, secretInformer informercorev1.SecretInformer,
	kubeClient kubernetes.Interface, readyGameServerCache *ReadyGameServerCache) *Allocator {
	ah := &Allocator{
		pendingRequests:        make(chan request, maxBatchQueue),
		allocationPolicyLister: policyInformer.Lister(),
		allocationPolicySynced: policyInformer.Informer().HasSynced,
		secretLister:           secretInformer.Lister(),
		secretSynced:           secretInformer.Informer().HasSynced,
		readyGameServerCache:   readyGameServerCache,
		topNGameServerCount:    topNGameServerDefaultCount,
	}

	ah.baseLogger = runtime.NewLoggerWithType(ah)
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(ah.baseLogger.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	ah.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "GameServerAllocation-Allocator"})

	return ah
}

// Start initiates the listeners.
func (c *Allocator) Start(stop <-chan struct{}) error {
	if err := c.Sync(stop); err != nil {
		return err
	}

	if err := c.readyGameServerCache.Start(stop); err != nil {
		return err
	}

	// workers and logic for batching allocations
	go c.ListenAndAllocate(maxBatchQueue, stop)

	return nil
}

// Sync waits for cache to sync
func (c *Allocator) Sync(stop <-chan struct{}) error {
	c.baseLogger.Info("Wait for Allocator cache sync")
	if !cache.WaitForCacheSync(stop, c.secretSynced, c.allocationPolicySynced) {
		return errors.New("failed to wait for caches to sync")
	}
	return nil
}

// Allocate CRDHandler for allocating a gameserver.
func (c *Allocator) Allocate(gsa *allocationv1.GameServerAllocation, stop <-chan struct{}) (k8sruntime.Object, error) {
	// server side validation
	if causes, ok := gsa.Validate(); !ok {
		status := &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: fmt.Sprintf("GameServerAllocation is invalid: Invalid value: %#v", gsa),
			Reason:  metav1.StatusReasonInvalid,
			Details: &metav1.StatusDetails{
				Kind:   "GameServerAllocation",
				Group:  allocationv1.SchemeGroupVersion.Group,
				Causes: causes,
			},
			Code: http.StatusUnprocessableEntity,
		}

		var gvks []schema.GroupVersionKind
		gvks, _, err := apiserver.Scheme.ObjectKinds(status)
		if err != nil {
			return nil, errors.Wrap(err, "could not find objectkinds for status")
		}

		status.TypeMeta = metav1.TypeMeta{Kind: gvks[0].Kind, APIVersion: gvks[0].Version}
		return status, nil
	}

	// If multi-cluster setting is enabled, allocate base on the multicluster allocation policy.
	var out *allocationv1.GameServerAllocation
	var err error
	if gsa.Spec.MultiClusterSetting.Enabled {
		out, err = c.applyMultiClusterAllocation(gsa, stop)
	} else {
		out, err = c.allocateFromLocalCluster(gsa, stop)
	}

	if err != nil {
		return nil, err
	}

	return out, nil
}

func (c *Allocator) loggerForGameServerAllocationKey(key string) *logrus.Entry {
	return logfields.AugmentLogEntry(c.baseLogger, logfields.GameServerAllocationKey, key)
}

func (c *Allocator) loggerForGameServerAllocation(gsa *allocationv1.GameServerAllocation) *logrus.Entry {
	gsaName := "NilGameServerAllocation"
	if gsa != nil {
		gsaName = gsa.Namespace + "/" + gsa.Name
	}
	return c.loggerForGameServerAllocationKey(gsaName).WithField("gsa", gsa)
}

// allocateFromLocalCluster allocates gameservers from the local cluster.
func (c *Allocator) allocateFromLocalCluster(gsa *allocationv1.GameServerAllocation, stop <-chan struct{}) (*allocationv1.GameServerAllocation, error) {
	var gs *agonesv1.GameServer
	err := Retry(allocationRetry, func() error {
		var err error
		gs, err = c.allocate(gsa, stop)
		return err
	})

	if err != nil && err != ErrNoGameServerReady && err != ErrConflictInGameServerSelection {
		c.readyGameServerCache.Resync()
		return nil, err
	}

	if err == ErrNoGameServerReady {
		gsa.Status.State = allocationv1.GameServerAllocationUnAllocated
	} else if err == ErrConflictInGameServerSelection {
		gsa.Status.State = allocationv1.GameServerAllocationContention
	} else {
		gsa.ObjectMeta.Name = gs.ObjectMeta.Name
		gsa.Status.State = allocationv1.GameServerAllocationAllocated
		gsa.Status.GameServerName = gs.ObjectMeta.Name
		gsa.Status.Ports = gs.Status.Ports
		gsa.Status.Address = gs.Status.Address
		gsa.Status.NodeName = gs.Status.NodeName
	}

	c.loggerForGameServerAllocation(gsa).Info("game server allocation")
	return gsa, nil
}

// applyMultiClusterAllocation retrieves allocation policies and iterate on policies.
// Then allocate gameservers from local or remote cluster accordingly.
func (c *Allocator) applyMultiClusterAllocation(gsa *allocationv1.GameServerAllocation, stop <-chan struct{}) (result *allocationv1.GameServerAllocation, err error) {
	selector := labels.Everything()
	if len(gsa.Spec.MultiClusterSetting.PolicySelector.MatchLabels)+len(gsa.Spec.MultiClusterSetting.PolicySelector.MatchExpressions) != 0 {
		selector, err = metav1.LabelSelectorAsSelector(&gsa.Spec.MultiClusterSetting.PolicySelector)
		if err != nil {
			return nil, err
		}
	}

	policies, err := c.allocationPolicyLister.GameServerAllocationPolicies(gsa.ObjectMeta.Namespace).List(selector)
	if err != nil {
		return nil, err
	} else if len(policies) == 0 {
		return nil, errors.New("no multi-cluster allocation policy is specified")
	}

	it := multiclusterv1alpha1.NewConnectionInfoIterator(policies)
	for {
		connectionInfo := it.Next()
		if connectionInfo == nil {
			break
		}
		if connectionInfo.ClusterName == gsa.ObjectMeta.ClusterName {
			result, err = c.allocateFromLocalCluster(gsa, stop)
			c.baseLogger.Error(err)
		} else {
			result, err = c.allocateFromRemoteCluster(*gsa, connectionInfo, gsa.ObjectMeta.Namespace)
			c.baseLogger.Error(err)
		}
		if result != nil {
			return result, nil
		}
	}
	return nil, err
}

// allocateFromRemoteCluster allocates gameservers from a remote cluster by making
// an http call to allocation service in that cluster.
func (c *Allocator) allocateFromRemoteCluster(gsa allocationv1.GameServerAllocation, connectionInfo *multiclusterv1alpha1.ClusterConnectionInfo, namespace string) (*allocationv1.GameServerAllocation, error) {
	var gsaResult allocationv1.GameServerAllocation

	// TODO: handle converting error to apiserver error
	// TODO: cache the client
	client, err := c.createRemoteClusterRestClient(namespace, connectionInfo.SecretName)
	if err != nil {
		return nil, err
	}

	// Forward the game server allocation request to another cluster,
	// and disable multicluster settings to avoid the target cluster
	// forward the allocation request again.
	gsa.Spec.MultiClusterSetting.Enabled = false
	gsa.Namespace = connectionInfo.Namespace
	body, err := json.Marshal(gsa)
	if err != nil {
		return nil, err
	}

	// TODO: Retry on transient error --> response.StatusCode >= 500
	for i, endpoint := range connectionInfo.AllocationEndpoints {
		response, err := client.Post(endpoint, "application/json", bytes.NewBuffer(body))
		if err != nil {
			return nil, err
		}
		defer response.Body.Close() // nolint: errcheck

		data, err := ioutil.ReadAll(response.Body)
		if err != nil {
			return nil, err
		}
		// If there are multiple enpoints for the allocator connection and the current one is
		// failing with 5xx http status, try the next endpoint. Otherwise, return the error response.
		if response.StatusCode >= 500 && (i+1) < len(connectionInfo.AllocationEndpoints) {
			// If there is a server error try a different endpoint
			c.baseLogger.WithError(err).WithField("endpoint", endpoint).Warn("The request sent failed, trying next endpoint")
			continue
		}
		if response.StatusCode >= 400 {
			// For error responses return the body without deserializing to an object.
			return nil, errors.New(string(data))
		}

		err = json.Unmarshal(data, &gsaResult)
		if err != nil {
			return nil, err
		}
		break
	}
	return &gsaResult, nil
}

// createRemoteClusterRestClient creates a rest client with proper certs to make a remote call.
func (c *Allocator) createRemoteClusterRestClient(namespace, secretName string) (*http.Client, error) {
	clientCert, clientKey, caCert, err := c.getClientCertificates(namespace, secretName)
	if err != nil {
		return nil, err
	}
	if clientCert == nil || clientKey == nil {
		return nil, fmt.Errorf("missing client certificate key pair in secret %s", secretName)
	}

	// Load client cert
	cert, err := tls.X509KeyPair(clientCert, clientKey)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}
	if len(caCert) != 0 {
		// Load CA cert, if provided and trust the server certificate.
		// This is required for self-signed certs.
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(caCert) {
			return nil, errors.New("only PEM format is accepted for server CA")
		}
	}

	// Setup HTTPS client
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}, nil
}

// getClientCertificates returns the client certificates and CA cert for remote allocation cluster call
func (c *Allocator) getClientCertificates(namespace, secretName string) (clientCert, clientKey, caCert []byte, err error) {
	secret, err := c.secretLister.Secrets(namespace).Get(secretName)
	if err != nil {
		return nil, nil, nil, err
	}
	if secret == nil || len(secret.Data) == 0 {
		return nil, nil, nil, fmt.Errorf("secert %s does not have data", secretName)
	}

	// Create http client using cert
	clientCert = secret.Data[secretClientCertName]
	clientKey = secret.Data[secretClientKeyName]
	caCert = secret.Data[secretCaCertName]
	return clientCert, clientKey, caCert, nil
}

// allocate allocated a GameServer from a given GameServerAllocation
// this sets up allocation through a batch process.
func (c *Allocator) allocate(gsa *allocationv1.GameServerAllocation, stop <-chan struct{}) (*agonesv1.GameServer, error) {
	// creates an allocation request. This contains the requested GameServerAllocation, as well as the
	// channel we expect the return values to come back for this GameServerAllocation
	req := request{gsa: gsa, response: make(chan response)}

	// this pushes the request into the batching process
	c.pendingRequests <- req

	select {
	case res := <-req.response: // wait for the batch to be completed
		return res.gs, res.err
	case <-stop:
		return nil, errors.New("shutting down")
	}
}

// ListenAndAllocate is a blocking function that runs in a loop
// looking at c.requestBatches for batches of requests that are coming through.
func (c *Allocator) ListenAndAllocate(updateWorkerCount int, stop <-chan struct{}) {
	// setup workers for allocation updates. Push response values into
	// this queue for concurrent updating of GameServers to Allocated
	updateQueue := c.allocationUpdateWorkers(updateWorkerCount, stop)

	// Batch processing strategy:
	// We constantly loop around the below for loop. If nothing is found in c.pendingRequests, we move to
	// default: which will wait for half a second, to allow for some requests to backup in c.pendingRequests,
	// providing us with a batch of Allocation requests in that channel

	// Once we have 1 or more requests in c.pendingRequests (which is buffered to 100), we can start the batch process.

	// Assuming this is the first run (either entirely, or for a while), list will be nil, and therefore the first
	// thing that will be done is retrieving the Ready GameSerers and sorting them for this batch via
	// c.listSortedReadyGameServers(). This list is maintained as we flow through the batch.

	// We then use findGameServerForAllocation to loop around the sorted list of Ready GameServers to look for matches
	// against the preferred and required selectors of the GameServerAllocation. If there is an error, we immediately
	// pass that straight back to the response channel for this GameServerAllocation.

	// Assuming we find a matching GameServer to our GameServerAllocation, we remove it from the list and the backing
	// Ready GameServer cache.

	// We then pass the found GameServers into the updateQueue, where there are updateWorkerCount number of goroutines
	// waiting to concurrently attempt to move the GameServer into an Allocated state, and return the result to
	// GameServerAllocation request's response channel

	// Then we get the next item off the batch (c.pendingRequests), and do this all over again, but this time, we have
	// an already sorted list of GameServers, so we only need to find one that matches our GameServerAllocation
	// selectors, and put it into updateQueue

	// The tracking of requestCount >= maxBatchBeforeRefresh is necessary, because without it, at high enough load
	// the list of GameServers that we are using to allocate would never get refreshed (list = nil) with an updated
	// list of Ready GameServers, and you would eventually never be able to Allocate anything as long as the load
	// continued.

	var list []*agonesv1.GameServer
	requestCount := 0

	for {
		select {
		case req := <-c.pendingRequests:
			// refresh the list after every 100 allocations made in a single batch
			requestCount++
			if requestCount >= maxBatchBeforeRefresh {
				list = nil
				requestCount = 0
			}

			if list == nil {
				list = c.readyGameServerCache.ListSortedReadyGameServers()
			}

			gs, index, err := findGameServerForAllocation(req.gsa, list)
			if err != nil {
				req.response <- response{request: req, gs: nil, err: err}
				continue
			}
			// remove the game server that has been allocated
			list = append(list[:index], list[index+1:]...)

			if err := c.readyGameServerCache.RemoveFromReadyGameServer(gs); err != nil {
				// this seems unlikely, but lets handle it just in case
				req.response <- response{request: req, gs: nil, err: err}
				continue
			}

			updateQueue <- response{request: req, gs: gs.DeepCopy(), err: nil}

		case <-stop:
			return
		default:
			list = nil
			requestCount = 0
			// slow down cpu churn, and allow items to batch
			time.Sleep(batchWaitTime)
		}
	}
}

// allocationUpdateWorkers runs workerCount number of goroutines as workers to
// process each GameServer passed into the returned updateQueue
// Each worker will concurrently attempt to move the GameServer to an Allocated
// state and then respond to the initial request's response channel with the
// details of that update
func (c *Allocator) allocationUpdateWorkers(workerCount int, stop <-chan struct{}) chan<- response {
	updateQueue := make(chan response)

	for i := 0; i < workerCount; i++ {
		go func() {
			for {
				select {
				case res := <-updateQueue:
					gs, err := c.readyGameServerCache.PatchGameServerMetadata(res.request.gsa.Spec.MetaPatch, *res.gs)
					if err != nil {
						// since we could not allocate, we should put it back
						c.readyGameServerCache.AddToReadyGameServer(gs)
						res.err = errors.Wrap(err, "error updating allocated gameserver")
					} else {
						res.gs = gs
						c.recorder.Event(res.gs, corev1.EventTypeNormal, string(res.gs.Status.State), "Allocated")
					}

					res.request.response <- res
				case <-stop:
					return
				}
			}
		}()
	}

	return updateQueue
}

// Retry retries fn based on backoff provided.
func Retry(backoff wait.Backoff, fn func() error) error {
	var lastConflictErr error
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		err := fn()
		switch {
		case err == nil:
			return true, nil
		case err == ErrNoGameServerReady:
			return true, err
		default:
			lastConflictErr = err
			return false, nil
		}
	})
	if err == wait.ErrWaitTimeout {
		err = lastConflictErr
	}
	return err
}

// getRandomlySelectedGS selects a GS from the set of Gameservers randomly. This will reduce the contentions
func (c *Allocator) getRandomlySelectedGS(gsa *allocationv1.GameServerAllocation, bestGSList []agonesv1.GameServer) *agonesv1.GameServer {
	seed, err := strconv.Atoi(gsa.ObjectMeta.ResourceVersion)
	if err != nil {
		seed = 1234567
	}

	ln := c.topNGameServerCount
	if ln > len(bestGSList) {
		ln = len(bestGSList)
	}

	startIndex := len(bestGSList) - ln
	bestGSList = bestGSList[startIndex:]
	index := rand.New(rand.NewSource(int64(seed))).Intn(ln)
	return &bestGSList[index]
}
