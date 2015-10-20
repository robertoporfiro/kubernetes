/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package prober

import (
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/probe"
	"k8s.io/kubernetes/pkg/util"
)

// worker handles the periodic probing of its assigned container. Each worker has a go-routine
// associated with it which runs the probe loop until the container permanently terminates, or the
// stop channel is closed. The worker uses the probe Manager's statusManager to get up-to-date
// container IDs.
type worker struct {
	// Channel for stopping the probe, it should be closed to trigger a stop.
	stop chan struct{}

	// The pod containing this probe (read-only)
	pod *api.Pod

	// The container to probe (read-only)
	container api.Container

	// Describes the probe configuration (read-only)
	spec *api.Probe

	// The type of the worker.
	probeType probeType

	// The probe value during the initial delay.
	initialValue results.Result

	// Where to store this workers results.
	resultsManager results.Manager
	probeManager   *manager

	// The last known container ID for this worker.
	containerID kubecontainer.ContainerID
}

// Creates and starts a new probe worker.
func newWorker(
	m *manager,
	probeType probeType,
	pod *api.Pod,
	container api.Container) *worker {

	w := &worker{
		stop:         make(chan struct{}),
		pod:          pod,
		container:    container,
		probeType:    probeType,
		probeManager: m,
	}

	switch probeType {
	case readiness:
		w.spec = container.ReadinessProbe
		w.resultsManager = m.readinessManager
		w.initialValue = results.Failure
	case liveness:
		w.spec = container.LivenessProbe
		w.resultsManager = m.livenessManager
		w.initialValue = results.Success
	}

	return w
}

// run periodically probes the container.
func (w *worker) run() {
	probeTicker := time.NewTicker(w.probeManager.defaultProbePeriod)

	defer func() {
		// Clean up.
		probeTicker.Stop()
		if !w.containerID.IsEmpty() {
			w.resultsManager.Remove(w.containerID)
		}

		w.probeManager.removeWorker(w.pod.UID, w.container.Name, w.probeType)
	}()

probeLoop:
	for w.doProbe() {
		// Wait for next probe tick.
		select {
		case <-w.stop:
			break probeLoop
		case <-probeTicker.C:
			// continue
		}
	}
}

// doProbe probes the container once and records the result.
// Returns whether the worker should continue.
func (w *worker) doProbe() (keepGoing bool) {
	defer util.HandleCrash(func(_ interface{}) { keepGoing = true })

	status, ok := w.probeManager.statusManager.GetPodStatus(w.pod.UID)
	if !ok {
		// Either the pod has not been created yet, or it was already deleted.
		glog.V(3).Infof("No status for pod: %v", kubeletutil.FormatPodName(w.pod))
		return true
	}

	// Worker should terminate if pod is terminated.
	if status.Phase == api.PodFailed || status.Phase == api.PodSucceeded {
		glog.V(3).Infof("Pod %v %v, exiting probe worker",
			kubeletutil.FormatPodName(w.pod), status.Phase)
		return false
	}

	c, ok := api.GetContainerStatus(status.ContainerStatuses, w.container.Name)
	if !ok {
		// Either the container has not been created yet, or it was deleted.
		glog.V(3).Infof("Non-existant container probed: %v - %v",
			kubeletutil.FormatPodName(w.pod), w.container.Name)
		return true // Wait for more information.
	}

	if w.containerID.String() != c.ContainerID {
		if !w.containerID.IsEmpty() {
			w.resultsManager.Remove(w.containerID)
		}
		w.containerID = kubecontainer.ParseContainerID(c.ContainerID)
	}

	if c.State.Running == nil {
		glog.V(3).Infof("Non-running container probed: %v - %v",
			kubeletutil.FormatPodName(w.pod), w.container.Name)
		if !w.containerID.IsEmpty() {
			w.resultsManager.Set(w.containerID, results.Failure, w.pod)
		}
		// Abort if the container will not be restarted.
		return c.State.Terminated == nil ||
			w.pod.Spec.RestartPolicy != api.RestartPolicyNever
	}

	if int64(time.Since(c.State.Running.StartedAt.Time).Seconds()) < w.spec.InitialDelaySeconds {
		w.resultsManager.Set(w.containerID, w.initialValue, w.pod)
		return true
	}

	// TODO: Move error handling out of prober.
	result, _ := w.probeManager.prober.probe(w.probeType, w.pod, status, w.container, w.containerID)
	if result != probe.Unknown {
		w.resultsManager.Set(w.containerID, result != probe.Failure, w.pod)
	}

	return true
}
