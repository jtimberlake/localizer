// Copyright 2020 Jared Allard
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
package proxier

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/getoutreach/localizer/pkg/hostsfile"
	"github.com/metal-stack/go-ipam"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type worker struct {
	k    kubernetes.Interface
	rest *rest.Config
	log  logrus.FieldLogger

	ippool ipam.Ipamer
	ipCidr string
	dns    *hostsfile.File

	reqChan  chan PortForwardRequest
	doneChan chan<- struct{}

	// portForwards are existing port-forwards
	portForwards map[string]*PortForwardConnection

	// lastTouchTime is the the worker has done any work, whether it
	// be creating, releasing, or updating port-forwards. The mutex
	// proceeding it is used to protect this value from concurrent
	// access.
	lastTouchTime time.Time
	touchMu       sync.Mutex
}

// NewPortForwarder creates a new port-forward worker that handles
// creating port-forwards and destroying port-forwards.
//nolint:gocritic,golint // We're OK not naming these.
func NewPortForwarder(ctx context.Context, k kubernetes.Interface,
	r *rest.Config, log logrus.FieldLogger, opts *ProxyOpts) (chan<- PortForwardRequest, <-chan struct{}, *worker, error) {
	ipamInstance := ipam.New()

	_, cidr, err := net.ParseCIDR(opts.IPCidr)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to parse provided cidr")
	}

	prefix, err := ipamInstance.NewPrefix(opts.IPCidr)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to create ip pool")
	}

	defaultIP := "127.0.0.1"
	if cidr.Contains(net.ParseIP(defaultIP)) {
		_, err = ipamInstance.AcquireSpecificIP(prefix.Cidr, defaultIP)
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "failed to create ip pool")
		}
	}

	hosts, err := hostsfile.New("", "")
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to open up hosts file for r/w")
	}

	doneChan := make(chan struct{})
	reqChan := make(chan PortForwardRequest, 1024)

	w := &worker{
		k:             k,
		rest:          r,
		log:           log,
		ippool:        ipamInstance,
		ipCidr:        prefix.Cidr,
		dns:           hosts,
		reqChan:       reqChan,
		doneChan:      doneChan,
		portForwards:  make(map[string]*PortForwardConnection),
		lastTouchTime: time.Now(),
	}

	go w.Start(ctx)

	return reqChan, doneChan, w, nil
}

// Start starts the worker process. This is done when the worker is created
// and should be run in a goroutine if this is created manually.
func (w *worker) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			for info := range w.portForwards {
				err := w.DeletePortForward(ctx, &DeletePortForwardRequest{
					Service: w.portForwards[info].Service,
				})
				if err != nil {
					w.log.WithError(err).Warn("failed to clean up port-forward")
				}
			}

			// close our channel(s)
			close(w.doneChan)

			return
		case req := <-w.reqChan:
			var serv ServiceInfo
			var err error
			if req.CreatePortForwardRequest != nil {
				err = w.CreatePortForward(ctx, req.CreatePortForwardRequest)
				serv = req.CreatePortForwardRequest.Service
			} else if req.DeletePortForwardRequest != nil {
				err = w.DeletePortForward(ctx, req.DeletePortForwardRequest)
				serv = req.DeletePortForwardRequest.Service
			}

			log := w.log.WithField("service", serv.Key())

			if err != nil {
				log.WithError(err).Errorf("encountered an error: %v", err)
			}
		}
	}
}

// touch notes that the worker is being touched by the proxier.
func (w *worker) touch() {
	w.touchMu.Lock()
	defer w.touchMu.Unlock()

	w.lastTouchTime = time.Now()
}

// isStable returns true if the worker hasn't been touched in 2 seconds or
// longer, and returns false otherwise. This was inteded to be used to denote
// some sort of "readiness", as the worker will be constantly creating
// port-forwards when starting up, draining the initial queue created by
// the proxier.
func (w *worker) isStable() bool {
	w.touchMu.Lock()
	defer w.touchMu.Unlock()

	return time.Since(w.lastTouchTime) >= time.Second*2
}

// getPodForService finds the first available endpoint for a given service
func (w *worker) getPodForService(ctx context.Context, si *ServiceInfo) (PodInfo, error) {
	e, err := w.k.CoreV1().Endpoints(si.Namespace).Get(ctx, si.Name, metav1.GetOptions{})
	if err != nil {
		return PodInfo{}, err
	}

	found := false
	pod := PodInfo{}

loop:
	for _, subset := range e.Subsets {
		for _, addr := range subset.Addresses {
			if addr.TargetRef == nil {
				continue
			}

			if addr.TargetRef.Kind != PodKind {
				continue
			}

			found = true
			pod.Name = addr.TargetRef.Name
			pod.Namespace = addr.TargetRef.Namespace

			break loop
		}
	}
	if !found {
		return pod, fmt.Errorf("failed to find endpoint for service")
	}

	return pod, nil
}

func (w *worker) CreatePortForward(ctx context.Context, req *CreatePortForwardRequest) (returnedError error) { //nolint:funlen,gocyclo
	serviceKey := req.Service.Key()
	log := w.log.WithField("service", serviceKey)
	if req.Endpoint != nil {
		log = log.WithField("endpoint", req.Endpoint.Key())
	}

	// skip port-forwards that are already being managed
	// unless it's marked as being recreated
	if _, ok := w.portForwards[serviceKey]; ok && !req.Recreate {
		return fmt.Errorf("already have a port-forward for this service")
	}

	// The worker is doing meaningful work, not a no-op, note this.
	w.touch()

	if req.Recreate {
		log.Infof("recreating port-forward due to: %v", req.RecreateReason)
		w.setPortForwardConnectionStatus(ctx, req.Service, PortForwardStatusRecreating, req.RecreateReason)
		err := w.stopPortForward(ctx, w.portForwards[serviceKey])
		if err != nil {
			log.WithError(err).Warn("failed to cleanup previous port-forward")
		}
	}

	pf := &PortForwardConnection{
		Service: req.Service,
		Status:  PortForwardStatusRunning,
		Ports:   req.Ports,
	}

	// cleanup after failed tunnel (that failed to be created)
	// using named returns we can check if an error occurred
	defer func() {
		if returnedError != nil {
			if err := w.stopPortForward(ctx, pf); err != nil {
				log.WithError(err).Warn("failed to cleanup failed tunnel")
			}
		}
	}()

	// TODO: need to release on error
	ipAddress, err := w.ippool.AcquireIP(w.ipCidr)
	if err != nil {
		return errors.Wrap(err, "failed to allocate IP")
	}
	pf.IP = ipAddress.IP.IPAddr().IP

	// We only need to create alias on darwin, on other platforms
	// lo0 becomes lo and routes the full /8
	if runtime.GOOS == "darwin" && os.Getenv("DISABLE_LOOPBACK_ALIAS") == "" {
		args := []string{"lo0", "alias", ipAddress.IP.String(), "up"}
		//nolint:govet // Why: We're OK shadowing err
		if err := exec.Command("ifconfig", args...).Run(); err != nil {
			return errors.Wrap(err, "failed to create ip link")
		}
	}
	pf.Hostnames = req.Hostnames

	//nolint:govet // Why: We're OK shadowing err
	if err := w.dns.AddHosts(ipAddress.IP.String(), req.Hostnames); err != nil {
		return errors.Wrap(err, "failed to add host entry")
	}

	//nolint:govet // Why: We're OK shadowing err
	if err := w.dns.Save(ctx); err != nil {
		return errors.Wrap(err, "failed to save host changes")
	}

	transport, upgrader, err := spdy.RoundTripperFor(w.rest)
	if err != nil {
		return errors.Wrap(err, "failed to upgrade connection")
	}

	var pod *PodInfo
	if req.Endpoint == nil {
		podInfo, err := w.getPodForService(ctx, &req.Service)
		if err == nil {
			pod = &podInfo
		}
	} else {
		pod = req.Endpoint
	}

	// only create the tunnel if we found a pod, if we didn't
	// then it will be looked for by the reaper
	if pod != nil {
		log = log.WithField("endpoint", pod.Key())
		pf.Pod = *pod

		log.Info("creating tunnel")
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", w.k.CoreV1().RESTClient().Post().
			Resource("pods").
			Namespace(pod.Namespace).
			Name(pod.Name).
			SubResource("portforward").URL())

		fw, err := portforward.NewOnAddresses(dialer, []string{ipAddress.IP.String()}, req.Ports, ctx.Done(), nil, ioutil.Discard, ioutil.Discard)
		if err != nil {
			return errors.Wrap(err, "failed to create port-forward")
		}
		pf.pf = fw

		go func() {
			err := fw.ForwardPorts()

			// if context was canceled (exiting) then we can ignore the error
			select {
			case <-ctx.Done():
				return
			default:
			}

			// otherwise, recreate it
			w.reqChan <- PortForwardRequest{
				CreatePortForwardRequest: &CreatePortForwardRequest{
					Service:        req.Service,
					Hostnames:      req.Hostnames,
					Ports:          req.Ports,
					Recreate:       true,
					RecreateReason: fmt.Sprintf("%v", err),
				},
			}
		}()
	} else {
		log.Warn("skipping tunnel creation due to no endpoint being found")
		pf.Status = PortForwardStatusWaiting
		pf.StatusReason = "No endpoints were found."
		if err := w.stopPortForward(ctx, pf); err != nil {
			return err
		}
	}

	// mark that this is allocated
	w.portForwards[req.Service.Key()] = pf

	return nil
}

func (w *worker) setPortForwardConnectionStatus(_ context.Context, si ServiceInfo, status PortForwardStatus, reason string) {
	key := si.Key()
	pf, ok := w.portForwards[key]
	if !ok {
		return
	}

	pf.Status = status
	pf.StatusReason = reason
	w.portForwards[key] = pf
}

func (w *worker) stopPortForward(_ context.Context, conn *PortForwardConnection) error {
	if conn.pf != nil {
		conn.pf.Close()
	}

	errs := make([]error, 0)
	if len(conn.IP) > 0 {
		// If we are on a platform that needs aliases
		// then we need to remove it
		if runtime.GOOS == "darwin" && os.Getenv("DISABLE_LOOPBACK_ALIAS") == "" {
			ipStr := conn.IP.String()
			args := []string{"lo0", "-alias", ipStr}
			if err := exec.Command("ifconfig", args...).Run(); err != nil {
				message := ""
				if exitError, ok := err.(*exec.ExitError); ok {
					message = string(exitError.Stderr)
				}
				errs = append(errs, errors.Wrapf(err, "failed to release ip alias: %s", message))
			}
		}

		err := w.ippool.ReleaseIPFromPrefix(w.ipCidr, conn.IP.String())
		if err != nil {
			errs = append(errs, errors.Wrap(err, "failed to release ip address"))
		}

		if err := w.dns.RemoveAddress(conn.IP.String()); err != nil {
			errs = append(errs, errors.Wrap(err, "failed to remove ip address from hostsfile"))
		}

		// We don't use the context provided because if it's canceled we need to be able to remove it still
		if err := w.dns.Save(context.Background()); err != nil {
			errs = append(errs, errors.Wrap(err, "failed to save hosts file after modification(s)"))
		}

		conn.IP = net.IP{}
	}

	// if we have errors, return them
	if len(errs) > 0 {
		strs := []string{}
		for _, err := range errs {
			strs = append(strs, err.Error())
		}
		return fmt.Errorf("%v", strs)
	}

	return nil
}

func (w *worker) DeletePortForward(ctx context.Context, req *DeletePortForwardRequest) error {
	serviceKey := req.Service.Key()
	log := w.log.WithField("service", serviceKey)

	// nothing to do for non exiting forwards.
	if w.portForwards[serviceKey] == nil {
		return nil
	}

	// The worker is doing meaningful work, not a no-op, note this.
	w.touch()

	if err := w.stopPortForward(ctx, w.portForwards[serviceKey]); err != nil {
		log.WithError(err).Warn("failed to cleanup port-forward")
	}

	// now mark it as not being allocated
	delete(w.portForwards, serviceKey)

	log.Info("stopped port-forward")

	return nil
}
