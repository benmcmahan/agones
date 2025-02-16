// Copyright 2018 Google LLC All Rights Reserved.
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

package gameservers

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"agones.dev/agones/pkg/apis/agones"
	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"agones.dev/agones/pkg/client/clientset/versioned"
	getterv1 "agones.dev/agones/pkg/client/clientset/versioned/typed/agones/v1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1 "agones.dev/agones/pkg/client/listers/agones/v1"
	"agones.dev/agones/pkg/util/crd"
	"agones.dev/agones/pkg/util/logfields"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/webhooks"
	"agones.dev/agones/pkg/util/workerqueue"
	"github.com/heptiolabs/healthcheck"
	"github.com/mattbaird/jsonpatch"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

// Controller is a the main GameServer crd controller
type Controller struct {
	baseLogger             *logrus.Entry
	sidecarImage           string
	alwaysPullSidecarImage bool
	sidecarCPURequest      resource.Quantity
	sidecarCPULimit        resource.Quantity
	sdkServiceAccount      string
	crdGetter              v1beta1.CustomResourceDefinitionInterface
	podGetter              typedcorev1.PodsGetter
	podLister              corelisterv1.PodLister
	podSynced              cache.InformerSynced
	gameServerGetter       getterv1.GameServersGetter
	gameServerLister       listerv1.GameServerLister
	gameServerSynced       cache.InformerSynced
	nodeLister             corelisterv1.NodeLister
	nodeSynced             cache.InformerSynced
	portAllocator          *PortAllocator
	healthController       *HealthController
	workerqueue            *workerqueue.WorkerQueue
	creationWorkerQueue    *workerqueue.WorkerQueue // handles creation only
	deletionWorkerQueue    *workerqueue.WorkerQueue // handles deletion only
	stop                   <-chan struct{}
	recorder               record.EventRecorder
}

// NewController returns a new gameserver crd controller
func NewController(
	wh *webhooks.WebHook,
	health healthcheck.Handler,
	minPort, maxPort int32,
	sidecarImage string,
	alwaysPullSidecarImage bool,
	sidecarCPURequest resource.Quantity,
	sidecarCPULimit resource.Quantity,
	sdkServiceAccount string,
	kubeClient kubernetes.Interface,
	kubeInformerFactory informers.SharedInformerFactory,
	extClient extclientset.Interface,
	agonesClient versioned.Interface,
	agonesInformerFactory externalversions.SharedInformerFactory) *Controller {

	pods := kubeInformerFactory.Core().V1().Pods()
	gameServers := agonesInformerFactory.Agones().V1().GameServers()
	gsInformer := gameServers.Informer()

	c := &Controller{
		sidecarImage:           sidecarImage,
		sidecarCPULimit:        sidecarCPULimit,
		sidecarCPURequest:      sidecarCPURequest,
		alwaysPullSidecarImage: alwaysPullSidecarImage,
		sdkServiceAccount:      sdkServiceAccount,
		crdGetter:              extClient.ApiextensionsV1beta1().CustomResourceDefinitions(),
		podGetter:              kubeClient.CoreV1(),
		podLister:              pods.Lister(),
		podSynced:              pods.Informer().HasSynced,
		gameServerGetter:       agonesClient.AgonesV1(),
		gameServerLister:       gameServers.Lister(),
		gameServerSynced:       gsInformer.HasSynced,
		nodeLister:             kubeInformerFactory.Core().V1().Nodes().Lister(),
		nodeSynced:             kubeInformerFactory.Core().V1().Nodes().Informer().HasSynced,
		portAllocator:          NewPortAllocator(minPort, maxPort, kubeInformerFactory, agonesInformerFactory),
		healthController:       NewHealthController(health, kubeClient, agonesClient, kubeInformerFactory, agonesInformerFactory),
	}

	c.baseLogger = runtime.NewLoggerWithType(c)

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(c.baseLogger.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	c.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "gameserver-controller"})

	c.workerqueue = workerqueue.NewWorkerQueueWithRateLimiter(c.syncGameServer, c.baseLogger, logfields.GameServerKey, agones.GroupName+".GameServerController", fastRateLimiter())
	c.creationWorkerQueue = workerqueue.NewWorkerQueueWithRateLimiter(c.syncGameServer, c.baseLogger.WithField("subqueue", "creation"), logfields.GameServerKey, agones.GroupName+".GameServerControllerCreation", fastRateLimiter())
	c.deletionWorkerQueue = workerqueue.NewWorkerQueueWithRateLimiter(c.syncGameServer, c.baseLogger.WithField("subqueue", "deletion"), logfields.GameServerKey, agones.GroupName+".GameServerControllerDeletion", fastRateLimiter())
	health.AddLivenessCheck("gameserver-workerqueue", healthcheck.Check(c.workerqueue.Healthy))
	health.AddLivenessCheck("gameserver-creation-workerqueue", healthcheck.Check(c.creationWorkerQueue.Healthy))
	health.AddLivenessCheck("gameserver-deletion-workerqueue", healthcheck.Check(c.deletionWorkerQueue.Healthy))

	wh.AddHandler("/mutate", agonesv1.Kind("GameServer"), admv1beta1.Create, c.creationMutationHandler)
	wh.AddHandler("/validate", agonesv1.Kind("GameServer"), admv1beta1.Create, c.creationValidationHandler)

	gsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueGameServerBasedOnState,
		UpdateFunc: func(oldObj, newObj interface{}) {
			// no point in processing unless there is a State change
			oldGs := oldObj.(*agonesv1.GameServer)
			newGs := newObj.(*agonesv1.GameServer)
			if oldGs.Status.State != newGs.Status.State || oldGs.ObjectMeta.DeletionTimestamp != newGs.ObjectMeta.DeletionTimestamp {
				c.enqueueGameServerBasedOnState(newGs)
			}
		},
	})

	// track pod deletions, for when GameServers are deleted
	pods.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			if isGameServerPod(oldPod) {
				newPod := newObj.(*corev1.Pod)
				//  node name has changed -- i.e. it has been scheduled
				if oldPod.Spec.NodeName != newPod.Spec.NodeName {
					owner := metav1.GetControllerOf(newPod)
					c.workerqueue.Enqueue(cache.ExplicitKey(newPod.ObjectMeta.Namespace + "/" + owner.Name))
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			// Could be a DeletedFinalStateUnknown, in which case, just ignore it
			pod, ok := obj.(*corev1.Pod)
			if ok && isGameServerPod(pod) {
				owner := metav1.GetControllerOf(pod)
				c.workerqueue.Enqueue(cache.ExplicitKey(pod.ObjectMeta.Namespace + "/" + owner.Name))
			}
		},
	})

	return c
}

func (c *Controller) enqueueGameServerBasedOnState(item interface{}) {
	gs := item.(*agonesv1.GameServer)

	switch gs.Status.State {
	case agonesv1.GameServerStatePortAllocation,
		agonesv1.GameServerStateCreating:
		c.creationWorkerQueue.Enqueue(gs)

	case agonesv1.GameServerStateShutdown:
		c.deletionWorkerQueue.Enqueue(gs)

	default:
		c.workerqueue.Enqueue(gs)
	}
}

// fastRateLimiter returns a fast rate limiter, without exponential back-off.
func fastRateLimiter() workqueue.RateLimiter {
	const numFastRetries = 5
	const fastDelay = 20 * time.Millisecond  // first few retries up to 'numFastRetries' are fast
	const slowDelay = 500 * time.Millisecond // subsequent retries are slow

	return workqueue.NewItemFastSlowRateLimiter(fastDelay, slowDelay, numFastRetries)
}

// creationMutationHandler is the handler for the mutating webhook that sets the
// the default values on the GameServer
// Should only be called on gameserver create operations.
// nolint:dupl
func (c *Controller) creationMutationHandler(review admv1beta1.AdmissionReview) (admv1beta1.AdmissionReview, error) {
	obj := review.Request.Object
	gs := &agonesv1.GameServer{}
	err := json.Unmarshal(obj.Raw, gs)
	if err != nil {
		c.baseLogger.WithField("review", review).WithError(err).Info("creationMutationHandler failed to unmarshal JSON")
		return review, errors.Wrapf(err, "error unmarshalling original GameServer json: %s", obj.Raw)
	}

	// This is the main logic of this function
	// the rest is really just json plumbing
	gs.ApplyDefaults()

	newGS, err := json.Marshal(gs)
	if err != nil {
		return review, errors.Wrapf(err, "error marshalling default applied GameSever %s to json", gs.ObjectMeta.Name)
	}

	patch, err := jsonpatch.CreatePatch(obj.Raw, newGS)
	if err != nil {
		return review, errors.Wrapf(err, "error creating patch for GameServer %s", gs.ObjectMeta.Name)
	}

	json, err := json.Marshal(patch)
	if err != nil {
		return review, errors.Wrapf(err, "error creating json for patch for GameServer %s", gs.ObjectMeta.Name)
	}

	c.loggerForGameServer(gs).WithField("patch", string(json)).Infof("patch created!")

	pt := admv1beta1.PatchTypeJSONPatch
	review.Response.PatchType = &pt
	review.Response.Patch = json

	return review, nil
}

func (c *Controller) loggerForGameServerKey(key string) *logrus.Entry {
	return logfields.AugmentLogEntry(c.baseLogger, logfields.GameServerKey, key)
}

func (c *Controller) loggerForGameServer(gs *agonesv1.GameServer) *logrus.Entry {
	gsName := "NilGameServer"
	if gs != nil {
		gsName = gs.Namespace + "/" + gs.Name
	}
	return c.loggerForGameServerKey(gsName).WithField("gs", gs)
}

// creationValidationHandler that validates a GameServer when it is created
// Should only be called on gameserver create operations.
func (c *Controller) creationValidationHandler(review admv1beta1.AdmissionReview) (admv1beta1.AdmissionReview, error) {
	obj := review.Request.Object
	gs := &agonesv1.GameServer{}
	err := json.Unmarshal(obj.Raw, gs)
	if err != nil {
		c.baseLogger.WithField("review", review).WithError(err).Info("creationValidationHandler failed to unmarshal JSON")
		return review, errors.Wrapf(err, "error unmarshalling original GameServer json: %s", obj.Raw)
	}

	c.loggerForGameServer(gs).WithField("review", review).Info("creationValidationHandler")

	causes, ok := gs.Validate()
	if !ok {
		review.Response.Allowed = false
		details := metav1.StatusDetails{
			Name:   review.Request.Name,
			Group:  review.Request.Kind.Group,
			Kind:   review.Request.Kind.Kind,
			Causes: causes,
		}
		review.Response.Result = &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: "GameServer configuration is invalid",
			Reason:  metav1.StatusReasonInvalid,
			Details: &details,
		}

		c.loggerForGameServer(gs).WithField("review", review).Info("Invalid GameServer")
		return review, nil
	}

	return review, nil
}

// Run the GameServer controller. Will block until stop is closed.
// Runs threadiness number workers to process the rate limited queue
func (c *Controller) Run(workers int, stop <-chan struct{}) error {
	c.stop = stop

	err := crd.WaitForEstablishedCRD(c.crdGetter, "gameservers.agones.dev", c.baseLogger)
	if err != nil {
		return err
	}

	c.baseLogger.Info("Wait for cache sync")
	if !cache.WaitForCacheSync(stop, c.gameServerSynced, c.podSynced, c.nodeSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	// Run the Port Allocator
	if err = c.portAllocator.Run(stop); err != nil {
		return errors.Wrap(err, "error running the port allocator")
	}

	// Run the Health Controller
	go func() {
		err = c.healthController.Run(stop)
		if err != nil {
			c.baseLogger.WithError(err).Error("error running health controller")
		}
	}()

	// start work queues
	var wg sync.WaitGroup

	startWorkQueue := func(wq *workerqueue.WorkerQueue) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wq.Run(workers, stop)
		}()
	}

	startWorkQueue(c.workerqueue)
	startWorkQueue(c.creationWorkerQueue)
	startWorkQueue(c.deletionWorkerQueue)
	wg.Wait()
	return nil
}

// syncGameServer synchronises the Pods for the GameServers.
// and reacts to status changes that can occur through the client SDK
func (c *Controller) syncGameServer(key string) error {
	c.loggerForGameServerKey(key).Info("Synchronising")

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// don't return an error, as we don't want this retried
		runtime.HandleError(c.loggerForGameServerKey(key), errors.Wrapf(err, "invalid resource key"))
		return nil
	}

	gs, err := c.gameServerLister.GameServers(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.loggerForGameServerKey(key).Info("GameServer is no longer available for syncing")
			return nil
		}
		return errors.Wrapf(err, "error retrieving GameServer %s from namespace %s", name, namespace)
	}

	if gs, err = c.syncGameServerDeletionTimestamp(gs); err != nil {
		return err
	}
	if gs, err = c.syncGameServerPortAllocationState(gs); err != nil {
		return err
	}
	if gs, err = c.syncGameServerCreatingState(gs); err != nil {
		return err
	}
	if gs, err = c.syncGameServerStartingState(gs); err != nil {
		return err
	}
	if gs, err = c.syncGameServerRequestReadyState(gs); err != nil {
		return err
	}
	if gs, err = c.syncDevelopmentGameServer(gs); err != nil {
		return err
	}
	if err = c.syncGameServerShutdownState(gs); err != nil {
		return err
	}

	return nil
}

// syncGameServerDeletionTimestamp if the deletion timestamp is non-zero
// then do one of two things:
// - if the GameServer has Pods running, delete them
// - if there no pods, remove the finalizer
func (c *Controller) syncGameServerDeletionTimestamp(gs *agonesv1.GameServer) (*agonesv1.GameServer, error) {
	if gs.ObjectMeta.DeletionTimestamp.IsZero() {
		return gs, nil
	}

	c.loggerForGameServer(gs).Info("Syncing with Deletion Timestamp")

	pod, err := c.gameServerPod(gs)
	if err != nil && !k8serrors.IsNotFound(err) {
		return gs, err
	}

	_, isDev := gs.GetDevAddress()
	if pod != nil && !isDev {
		// only need to do this once
		if pod.ObjectMeta.DeletionTimestamp.IsZero() {
			err = c.podGetter.Pods(pod.ObjectMeta.Namespace).Delete(pod.ObjectMeta.Name, nil)
			if err != nil {
				return gs, errors.Wrapf(err, "error deleting pod for GameServer %s, %s", gs.ObjectMeta.Name, pod.ObjectMeta.Name)
			}
			c.recorder.Event(gs, corev1.EventTypeNormal, string(gs.Status.State), fmt.Sprintf("Deleting Pod %s", pod.ObjectMeta.Name))
		}

		// but no removing finalizers until it's truly gone
		return gs, nil
	}

	gsCopy := gs.DeepCopy()
	// remove the finalizer for this controller
	var fin []string
	for _, f := range gsCopy.ObjectMeta.Finalizers {
		if f != agones.GroupName {
			fin = append(fin, f)
		}
	}
	gsCopy.ObjectMeta.Finalizers = fin
	c.loggerForGameServer(gsCopy).Infof("No pods found, removing finalizer %s", agones.GroupName)
	gs, err = c.gameServerGetter.GameServers(gsCopy.ObjectMeta.Namespace).Update(gsCopy)
	return gs, errors.Wrapf(err, "error removing finalizer for GameServer %s", gsCopy.ObjectMeta.Name)
}

// syncGameServerPortAllocationState gives a port to a dynamically allocating GameServer
func (c *Controller) syncGameServerPortAllocationState(gs *agonesv1.GameServer) (*agonesv1.GameServer, error) {
	if !(gs.Status.State == agonesv1.GameServerStatePortAllocation && gs.ObjectMeta.DeletionTimestamp.IsZero()) {
		return gs, nil
	}

	gsCopy := c.portAllocator.Allocate(gs.DeepCopy())

	gsCopy.Status.State = agonesv1.GameServerStateCreating
	c.recorder.Event(gs, corev1.EventTypeNormal, string(gs.Status.State), "Port allocated")

	c.loggerForGameServer(gsCopy).Info("Syncing Port Allocation GameServerState")
	gs, err := c.gameServerGetter.GameServers(gs.ObjectMeta.Namespace).Update(gsCopy)
	if err != nil {
		// if the GameServer doesn't get updated with the port data, then put the port
		// back in the pool, as it will get retried on the next pass
		c.portAllocator.DeAllocate(gsCopy)
		return gs, errors.Wrapf(err, "error updating GameServer %s to default values", gs.Name)
	}

	return gs, nil
}

// syncGameServerCreatingState checks if the GameServer is in the Creating state, and if so
// creates a Pod for the GameServer and moves the state to Starting
func (c *Controller) syncGameServerCreatingState(gs *agonesv1.GameServer) (*agonesv1.GameServer, error) {
	if !(gs.Status.State == agonesv1.GameServerStateCreating && gs.ObjectMeta.DeletionTimestamp.IsZero()) {
		return gs, nil
	}
	if _, isDev := gs.GetDevAddress(); isDev {
		return gs, nil
	}

	c.loggerForGameServer(gs).Info("Syncing Create State")

	// Maybe something went wrong, and the pod was created, but the state was never moved to Starting, so let's check
	_, err := c.gameServerPod(gs)
	if k8serrors.IsNotFound(err) {
		gs, err = c.createGameServerPod(gs)
		if err != nil || gs.Status.State == agonesv1.GameServerStateError {
			return gs, err
		}
	}

	if err != nil {
		return nil, errors.WithStack(err)
	}

	gsCopy := gs.DeepCopy()
	gsCopy.Status.State = agonesv1.GameServerStateStarting
	gs, err = c.gameServerGetter.GameServers(gs.ObjectMeta.Namespace).Update(gsCopy)
	if err != nil {
		return gs, errors.Wrapf(err, "error updating GameServer %s to Starting state", gs.Name)
	}
	return gs, nil
}

// syncDevelopmentGameServer manages advances a development gameserver to Ready status and registers its address and ports.
func (c *Controller) syncDevelopmentGameServer(gs *agonesv1.GameServer) (*agonesv1.GameServer, error) {
	// do not sync if the server is deleting.
	if !(gs.ObjectMeta.DeletionTimestamp.IsZero()) {
		return gs, nil
	}
	// Get the development IP address
	devIPAddress, isDevGs := gs.GetDevAddress()
	if !isDevGs {
		return gs, nil
	}

	if !(gs.Status.State == agonesv1.GameServerStateReady) {
		c.loggerForGameServer(gs).Info("GS is a development game server and will not be managed by Agones.")
	}

	gsCopy := gs.DeepCopy()
	var ports []agonesv1.GameServerStatusPort
	for _, p := range gs.Spec.Ports {
		ports = append(ports, p.Status())
	}
	// TODO: Use UpdateStatus() when it's available.
	gsCopy.Status.State = agonesv1.GameServerStateReady
	gsCopy.Status.Ports = ports
	gsCopy.Status.Address = devIPAddress
	gsCopy.Status.NodeName = devIPAddress
	gs, err := c.gameServerGetter.GameServers(gs.ObjectMeta.Namespace).Update(gsCopy)
	if err != nil {
		return gs, errors.Wrapf(err, "error updating GameServer %s to %v status", gs.Name, gs.Status)
	}
	return gs, nil
}

// createGameServerPod creates the backing Pod for a given GameServer
func (c *Controller) createGameServerPod(gs *agonesv1.GameServer) (*agonesv1.GameServer, error) {
	sidecar := c.sidecar(gs)
	var pod *corev1.Pod
	pod, err := gs.Pod(sidecar)

	// this shouldn't happen, but if it does.
	if err != nil {
		c.loggerForGameServer(gs).WithError(err).Error("error creating pod from Game Server")
		gs, err = c.moveToErrorState(gs, err.Error())
		return gs, err
	}

	// if the service account is not set, then you are in the "opinionated"
	// mode. If the user sets the service account, we assume they know what they are
	// doing, and don't disable the gameserver container.
	if pod.Spec.ServiceAccountName == "" {
		pod.Spec.ServiceAccountName = c.sdkServiceAccount
		gs.DisableServiceAccount(pod)
	}

	c.addGameServerHealthCheck(gs, pod)

	c.loggerForGameServer(gs).WithField("pod", pod).Info("creating Pod for GameServer")
	pod, err = c.podGetter.Pods(gs.ObjectMeta.Namespace).Create(pod)
	if k8serrors.IsAlreadyExists(err) {
		c.recorder.Event(gs, corev1.EventTypeNormal, string(gs.Status.State), "Pod already exists, reused")
		return gs, nil
	}
	if err != nil {
		if k8serrors.IsInvalid(err) {
			c.loggerForGameServer(gs).WithField("pod", pod).Errorf("Pod created is invalid")
			gs, err = c.moveToErrorState(gs, err.Error())
			return gs, err
		}
		return gs, errors.Wrapf(err, "error creating Pod for GameServer %s", gs.Name)
	}
	c.recorder.Event(gs, corev1.EventTypeNormal, string(gs.Status.State),
		fmt.Sprintf("Pod %s created", pod.ObjectMeta.Name))

	return gs, nil
}

// sidecar creates the sidecar container for a given GameServer
func (c *Controller) sidecar(gs *agonesv1.GameServer) corev1.Container {
	sidecar := corev1.Container{
		Name:  "agones-gameserver-sidecar",
		Image: c.sidecarImage,
		Env: []corev1.EnvVar{
			{
				Name:  "GAMESERVER_NAME",
				Value: gs.ObjectMeta.Name,
			},
			{
				Name: "POD_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
		},
		Resources: corev1.ResourceRequirements{},
		LivenessProbe: &corev1.Probe{
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(8080),
				},
			},
			InitialDelaySeconds: 3,
			PeriodSeconds:       3,
		},
	}

	if gs.Spec.SdkServer.GRPCPort != 0 {
		sidecar.Args = append(sidecar.Args, fmt.Sprintf("--grpc-port=%d", gs.Spec.SdkServer.GRPCPort))
	}

	if gs.Spec.SdkServer.HTTPPort != 0 {
		sidecar.Args = append(sidecar.Args, fmt.Sprintf("--http-port=%d", gs.Spec.SdkServer.HTTPPort))
	}

	if !c.sidecarCPURequest.IsZero() {
		sidecar.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: c.sidecarCPURequest}
	}

	if !c.sidecarCPULimit.IsZero() {
		sidecar.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: c.sidecarCPULimit}
	}

	if c.alwaysPullSidecarImage {
		sidecar.ImagePullPolicy = corev1.PullAlways
	}
	return sidecar
}

// addGameServerHealthCheck adds the http health check to the GameServer container
func (c *Controller) addGameServerHealthCheck(gs *agonesv1.GameServer, pod *corev1.Pod) {
	if gs.Spec.Health.Disabled {
		return
	}

	gs.ApplyToPodGameServerContainer(pod, func(c corev1.Container) corev1.Container {
		if c.LivenessProbe == nil {
			c.LivenessProbe = &corev1.Probe{
				Handler: corev1.Handler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/gshealthz",
						Port: intstr.FromInt(8080),
					},
				},
				InitialDelaySeconds: gs.Spec.Health.InitialDelaySeconds,
				PeriodSeconds:       gs.Spec.Health.PeriodSeconds,
				FailureThreshold:    gs.Spec.Health.FailureThreshold,
			}
		}

		return c
	})
}

// syncGameServerStartingState looks for a pod that has been scheduled for this GameServer
// and then sets the Status > Address and Ports values.
func (c *Controller) syncGameServerStartingState(gs *agonesv1.GameServer) (*agonesv1.GameServer, error) {
	if !(gs.Status.State == agonesv1.GameServerStateStarting && gs.ObjectMeta.DeletionTimestamp.IsZero()) {
		return gs, nil
	}
	if _, isDev := gs.GetDevAddress(); isDev {
		return gs, nil
	}

	c.loggerForGameServer(gs).Info("Syncing Starting GameServerState")

	// there should be a pod (although it may not have a scheduled container),
	// so if there is an error of any kind, then move this to queue backoff
	pod, err := c.gameServerPod(gs)
	if err != nil {
		return nil, err
	}

	gsCopy := gs.DeepCopy()
	// if we can't get the address, then go into queue backoff
	gsCopy, err = c.applyGameServerAddressAndPort(gsCopy, pod)
	if err != nil {
		return gs, err
	}

	gsCopy.Status.State = agonesv1.GameServerStateScheduled
	gs, err = c.gameServerGetter.GameServers(gs.ObjectMeta.Namespace).Update(gsCopy)
	if err != nil {
		return gs, errors.Wrapf(err, "error updating GameServer %s to Scheduled state", gs.Name)
	}
	c.recorder.Event(gs, corev1.EventTypeNormal, string(gs.Status.State), "Address and port populated")

	return gs, nil
}

// applyGameServerAddressAndPort gets the backing Pod for the GamesServer,
// and sets the allocated Address and Port values to it and returns it.
func (c *Controller) applyGameServerAddressAndPort(gs *agonesv1.GameServer, pod *corev1.Pod) (*agonesv1.GameServer, error) {
	addr, err := c.address(gs, pod)
	if err != nil {
		return gs, errors.Wrapf(err, "error getting external address for GameServer %s", gs.ObjectMeta.Name)
	}

	gs.Status.Address = addr
	gs.Status.NodeName = pod.Spec.NodeName
	// HostPort is always going to be populated, even when dynamic
	// This will be a double up of information, but it will be easier to read
	gs.Status.Ports = make([]agonesv1.GameServerStatusPort, len(gs.Spec.Ports))
	for i, p := range gs.Spec.Ports {
		gs.Status.Ports[i] = p.Status()
	}

	return gs, nil
}

// syncGameServerRequestReadyState checks if the Game Server is Requesting to be ready,
// and then adds the IP and Port information to the Status and marks the GameServer
// as Ready
func (c *Controller) syncGameServerRequestReadyState(gs *agonesv1.GameServer) (*agonesv1.GameServer, error) {
	if !(gs.Status.State == agonesv1.GameServerStateRequestReady && gs.ObjectMeta.DeletionTimestamp.IsZero()) ||
		gs.Status.State == agonesv1.GameServerStateUnhealthy {
		return gs, nil
	}
	if _, isDev := gs.GetDevAddress(); isDev {
		return gs, nil
	}

	c.loggerForGameServer(gs).Info("Syncing RequestReady State")

	gsCopy := gs.DeepCopy()

	// if the address hasn't been populated, and the Ready request comes
	// before the controller has had a chance to do it, then
	// do it here instead
	addressPopulated := false
	if gs.Status.NodeName == "" {
		addressPopulated = true
		pod, err := c.gameServerPod(gs)
		// NotFound should never happen, and if it does -- something bad happened,
		// so go into workerqueue backoff.
		if err != nil {
			return nil, err
		}
		gsCopy, err = c.applyGameServerAddressAndPort(gsCopy, pod)
		if err != nil {
			return gs, err
		}
	}

	gsCopy.Status.State = agonesv1.GameServerStateReady
	gs, err := c.gameServerGetter.GameServers(gs.ObjectMeta.Namespace).Update(gsCopy)
	if err != nil {
		return gs, errors.Wrapf(err, "error setting Ready, Port and address on GameServer %s Status", gs.ObjectMeta.Name)
	}

	if addressPopulated {
		c.recorder.Event(gs, corev1.EventTypeNormal, string(gs.Status.State), "Address and port populated")
	}
	c.recorder.Event(gs, corev1.EventTypeNormal, string(gs.Status.State), "SDK.Ready() complete")
	return gs, nil
}

// syncGameServerShutdownState deletes the GameServer (and therefore the backing Pod) if it is in shutdown state
func (c *Controller) syncGameServerShutdownState(gs *agonesv1.GameServer) error {
	if !(gs.Status.State == agonesv1.GameServerStateShutdown && gs.ObjectMeta.DeletionTimestamp.IsZero()) {
		return nil
	}

	c.loggerForGameServer(gs).Info("Syncing Shutdown State")
	// be explicit about where to delete.
	p := metav1.DeletePropagationBackground
	err := c.gameServerGetter.GameServers(gs.ObjectMeta.Namespace).Delete(gs.ObjectMeta.Name, &metav1.DeleteOptions{PropagationPolicy: &p})
	if err != nil {
		return errors.Wrapf(err, "error deleting Game Server %s", gs.ObjectMeta.Name)
	}
	c.recorder.Event(gs, corev1.EventTypeNormal, string(gs.Status.State), "Deletion started")
	return nil
}

// moveToErrorState moves the GameServer to the error state
func (c *Controller) moveToErrorState(gs *agonesv1.GameServer, msg string) (*agonesv1.GameServer, error) {
	copy := gs.DeepCopy()
	copy.Status.State = agonesv1.GameServerStateError

	gs, err := c.gameServerGetter.GameServers(gs.ObjectMeta.Namespace).Update(copy)
	if err != nil {
		return gs, errors.Wrapf(err, "error moving GameServer %s to Error State", gs.ObjectMeta.Name)
	}

	c.recorder.Event(gs, corev1.EventTypeWarning, string(gs.Status.State), msg)
	return gs, nil
}

// gameServerPod returns the Pod for this Game Server, or an error if there are none,
// or it cannot be determined (there are more than one, which should not happen)
func (c *Controller) gameServerPod(gs *agonesv1.GameServer) (*corev1.Pod, error) {
	// If the game server is a dev server we do not create a pod for it, return an empty pod.
	if _, isDev := gs.GetDevAddress(); isDev {
		return &corev1.Pod{}, nil
	}

	pod, err := c.podLister.Pods(gs.ObjectMeta.Namespace).Get(gs.ObjectMeta.Name)

	// if not found, propagate this error up, so we can use it in checks
	if k8serrors.IsNotFound(err) {
		return nil, err
	}

	if !metav1.IsControlledBy(pod, gs) {
		return nil, k8serrors.NewNotFound(corev1.Resource("pod"), gs.ObjectMeta.Name)
	}

	return pod, errors.Wrapf(err, "error retrieving pod for GameServer %s", gs.ObjectMeta.Name)
}

// address returns the IP that the given Pod is being run on
// This should be the externalIP, but if the externalIP is
// not set, it will fall back to the internalIP with a warning.
// (basically because minikube only has an internalIP)
func (c *Controller) address(gs *agonesv1.GameServer, pod *corev1.Pod) (string, error) {
	node, err := c.nodeLister.Get(pod.Spec.NodeName)
	if err != nil {
		return "", errors.Wrapf(err, "error retrieving node %s for Pod %s", pod.Spec.NodeName, pod.ObjectMeta.Name)
	}

	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeExternalIP && net.ParseIP(a.Address) != nil {
			return a.Address, nil
		}
	}

	// minikube only has an InternalIP on a Node, so we'll fall back to that.
	c.loggerForGameServer(gs).WithField("node", node.ObjectMeta.Name).Warn("Could not find ExternalIP. Falling back to Internal")
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP && net.ParseIP(a.Address) != nil {
			return a.Address, nil
		}
	}

	return "", errors.Errorf("Could not find an address for Node: %s", node.ObjectMeta.Name)
}

// isGameServerPod returns if this Pod is a Pod that comes from a GameServer
func isGameServerPod(pod *corev1.Pod) bool {
	if agonesv1.GameServerRolePodSelector.Matches(labels.Set(pod.ObjectMeta.Labels)) {
		owner := metav1.GetControllerOf(pod)
		return owner != nil && owner.Kind == "GameServer"
	}

	return false
}
