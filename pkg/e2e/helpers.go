/*
Copyright 2018 The Kubernetes Authors.

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

package e2e

import (
	"context"
	"fmt"
	"net"
	"time"

	"encoding/json"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	"io/ioutil"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/ingress-gce/cmd/echo/app"
	"k8s.io/ingress-gce/pkg/annotations"
	"k8s.io/ingress-gce/pkg/fuzz"
	"k8s.io/ingress-gce/pkg/fuzz/features"
	"k8s.io/klog"
	"net/http"
	"strings"
)

const (
	ingressPollInterval = 30 * time.Second
	ingressPollTimeout  = 25 * time.Minute

	gclbDeletionInterval = 30 * time.Second
	gclbDeletionTimeout  = 15 * time.Minute

	negPollInterval = 5 * time.Second
	negPollTimeout  = 2 * time.Minute

	k8sApiPoolInterval = 10 * time.Second
	k8sApiPollTimeout  = 30 * time.Minute

	healthyState = "HEALTHY"
)

// WaitForIngressOptions holds options dictating how we wait for an ingress to stabilize
type WaitForIngressOptions struct {
	// ExpectUnreachable is true when we expect the LB to still be
	// programming itself (i.e 404's / 502's)
	ExpectUnreachable bool
}

// IsRfc1918Addr returns true if the address supplied is an RFC1918 address
func IsRfc1918Addr(addr string) bool {
	ip := net.ParseIP(addr)
	var ipBlocks []*net.IPNet
	for _, cidr := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
	} {
		_, block, _ := net.ParseCIDR(cidr)
		ipBlocks = append(ipBlocks, block)
	}

	for _, block := range ipBlocks {
		if block.Contains(ip) {
			return true
		}
	}

	return false
}

// WaitForIngress to stabilize.
// We expect the ingress to be unreachable at first as LB is
// still programming itself (i.e 404's / 502's)
func WaitForIngress(s *Sandbox, ing *v1beta1.Ingress, options *WaitForIngressOptions) (*v1beta1.Ingress, error) {
	err := wait.Poll(ingressPollInterval, ingressPollTimeout, func() (bool, error) {
		var err error
		crud := IngressCRUD{s.f.Clientset}
		ing, err = crud.Get(s.Namespace, ing.Name)
		if err != nil {
			return true, err
		}
		validator, err := fuzz.NewIngressValidator(s.ValidatorEnv, ing, features.All, nil)
		if err != nil {
			return true, err
		}
		result := validator.Check(context.Background())
		if result.Err == nil {
			return true, nil
		}
		if options == nil || options.ExpectUnreachable {
			return false, nil
		}
		return true, fmt.Errorf("unexpected error from validation: %v", result.Err)
	})
	return ing, err
}

// WaitForIngressDeletion deletes the given ingress and waits for the
// resources associated with it to be deleted.
func WaitForIngressDeletion(ctx context.Context, g *fuzz.GCLB, s *Sandbox, ing *v1beta1.Ingress, options *fuzz.GCLBDeleteOptions) error {
	crud := IngressCRUD{s.f.Clientset}
	if err := crud.Delete(ing.Namespace, ing.Name); err != nil {
		return fmt.Errorf("delete(%q) = %v, want nil", ing.Name, err)
	}
	klog.Infof("Waiting for GCLB resources to be deleted (%s/%s), IngressDeletionOptions=%+v", s.Namespace, ing.Name, options)
	if err := WaitForGCLBDeletion(ctx, s.f.Cloud, g, options); err != nil {
		return fmt.Errorf("WaitForGCLBDeletion(...) = %v, want nil", err)
	}
	klog.Infof("GCLB resources deleted (%s/%s)", s.Namespace, ing.Name)
	return nil
}

// WaitForFinalizerDeletion waits for gclb resources to be deleted and
// the finalizer attached to the Ingress resource to be removed.
func WaitForFinalizerDeletion(ctx context.Context, g *fuzz.GCLB, s *Sandbox, ingName string, options *fuzz.GCLBDeleteOptions) error {
	klog.Infof("Waiting for GCLB resources to be deleted (%s/%s), IngressDeletionOptions=%+v", s.Namespace, ingName, options)
	if err := WaitForGCLBDeletion(ctx, s.f.Cloud, g, options); err != nil {
		return fmt.Errorf("WaitForGCLBDeletion(...) = %v, want nil", err)
	}
	klog.Infof("GCLB resources deleted (%s/%s)", s.Namespace, ingName)

	crud := IngressCRUD{s.f.Clientset}
	klog.Infof("Waiting for Finalizer to be removed for Ingress %s/%s", s.Namespace, ingName)
	return wait.Poll(k8sApiPoolInterval, k8sApiPollTimeout, func() (bool, error) {
		ing, err := crud.Get(s.Namespace, ingName)
		if err != nil {
			klog.Infof("WaitForFinalizerDeletion(%s/%s) = Error retrieving Ingress: %v", s.Namespace, ing.Name, err)
			return false, nil
		}
		if len(ing.GetFinalizers()) != 0 {
			klog.Infof("WaitForFinalizerDeletion(%s/%s) = %v", s.Namespace, ing.Name, ing.GetFinalizers())
			return false, nil
		}
		return true, nil
	})
}

// WaitForGCLBDeletion waits for the resources associated with the GLBC to be
// deleted.
func WaitForGCLBDeletion(ctx context.Context, c cloud.Cloud, g *fuzz.GCLB, options *fuzz.GCLBDeleteOptions) error {
	return wait.Poll(gclbDeletionInterval, gclbDeletionTimeout, func() (bool, error) {
		if err := g.CheckResourceDeletion(ctx, c, options); err != nil {
			klog.Infof("WaitForGCLBDeletion(%q) = %v", g.VIP, err)
			return false, nil
		}
		return true, nil
	})
}

// WaitForNEGDeletion waits for all NEGs associated with a GCLB to be deleted via GC
func WaitForNEGDeletion(ctx context.Context, c cloud.Cloud, g *fuzz.GCLB, options *fuzz.GCLBDeleteOptions) error {
	return wait.Poll(negPollInterval, gclbDeletionTimeout, func() (bool, error) {
		if err := g.CheckNEGDeletion(ctx, c, options); err != nil {
			klog.Infof("WaitForNegDeletion(%q) = %v", g.VIP, err)
			return false, nil
		}
		return true, nil
	})
}

// WaitForEchoDeploymentStable waits until the deployment's readyReplicas, availableReplicas and updatedReplicas are equal to replicas.
func WaitForEchoDeploymentStable(s *Sandbox, name string) error {
	return wait.Poll(k8sApiPoolInterval, k8sApiPollTimeout, func() (bool, error) {
		deployment, err := s.f.Clientset.AppsV1().Deployments(s.Namespace).Get(name, metav1.GetOptions{})
		if deployment == nil || err != nil {
			return false, fmt.Errorf("failed to get deployment %s/%s: %v", s.Namespace, name, err)
		}
		if err := CheckDeployment(deployment); err != nil {
			klog.Infof("WaitForEchoDeploymentStable(%s/%s) = %v", s.Namespace, name, err)
			return false, nil
		}
		return true, nil
	})
}

// WaitForNegStatus waits util the neg status on the service got to expected state.
func WaitForNegStatus(s *Sandbox, name string, expectSvcPorts []string) (annotations.NegStatus, error) {
	var ret annotations.NegStatus
	var err error
	err = wait.Poll(negPollInterval, gclbDeletionTimeout, func() (bool, error) {
		svc, err := s.f.Clientset.CoreV1().Services(s.Namespace).Get(name, metav1.GetOptions{})
		if svc == nil || err != nil {
			return false, fmt.Errorf("failed to get service %s/%s: %v", s.Namespace, name, err)
		}
		ret, err = CheckNegStatus(svc, expectSvcPorts)
		if err != nil {
			klog.Infof("WaitForNegStatus(%s/%s, %v) = %v", s.Namespace, name, expectSvcPorts, err)
			return false, nil
		}
		return true, nil
	})
	return ret, err
}

// WaitForNegs waits until the input NEG got into the expect states.
func WaitForNegs(ctx context.Context, c cloud.Cloud, negName string, zones []string, expectHealthy bool, expectCount int) error {
	return wait.Poll(negPollInterval, negPollTimeout, func() (bool, error) {
		negs, err := fuzz.NetworkEndpointsInNegs(ctx, c, negName, zones)
		if err != nil {
			return false, fmt.Errorf("failed to retrieve NEG %v from zones %v: %v", negName, zones, err)
		}

		if err := CheckNegs(negs, expectHealthy, expectCount); err != nil {
			klog.Infof("WaitForNegs(%q, %v, %v, %v) = %v", negName, zones, expectHealthy, expectCount, err)
			return false, nil
		}
		return true, nil
	})
}

// WaitForDistinctHosts waits util
func WaitForDistinctHosts(ctx context.Context, vip string, expectDistinctHosts int, tolerateTransientError bool) error {
	return wait.Poll(negPollInterval, negPollTimeout, func() (bool, error) {
		if err := CheckDistinctResponseHost(vip, expectDistinctHosts, tolerateTransientError); err != nil {
			klog.Infof("WaitForDistinctHosts(%q, %v, %v) = %v", vip, expectDistinctHosts, tolerateTransientError, err)
			return false, nil
		}
		return true, nil
	})
}

// CheckGCLB whitebox testing is OK.
func CheckGCLB(gclb *fuzz.GCLB, numForwardingRules int, numBackendServices int) error {
	// Do some cursory checks on the GCP objects.
	if len(gclb.ForwardingRule) != numForwardingRules {
		return fmt.Errorf("got %d forwarding rules, want %d", len(gclb.ForwardingRule), numForwardingRules)
	}
	if len(gclb.BackendService) != numBackendServices {
		return fmt.Errorf("got %d backend services, want %d", len(gclb.BackendService), numBackendServices)
	}

	return nil
}

// CheckDistinctResponseHost issue GET call to the vip for 100 times, parse the reponses and calculate the number of distinct backends.
func CheckDistinctResponseHost(vip string, expectDistinctHosts int, tolerateTransientError bool) error {
	var errs []error
	const repeat = 100
	hosts := sets.NewString()
	for i := 0; i < repeat; i++ {
		res, err := CheckEchoServerResponse(vip)
		if err != nil {
			if tolerateTransientError {
				klog.Infof("ignoring error from vip %q: %v. ", vip, err)
				continue
			}
			errs = append(errs, err)
		}
		hosts.Insert(res.K8sEnv.Pod)
	}
	if hosts.Len() != expectDistinctHosts {
		errs = append(errs, fmt.Errorf("got %v distinct hosts responsing vip %q, want %v", hosts.Len(), vip, expectDistinctHosts))
	}
	return utilerrors.NewAggregate(errs)
}

// CheckEchoServerResponse issue a GET call to the vip and return the ResponseBody.
func CheckEchoServerResponse(vip string) (app.ResponseBody, error) {
	url := fmt.Sprintf("http://%s/", vip)
	var body app.ResponseBody
	resp, err := http.Get(url)
	if err != nil {
		return body, fmt.Errorf("failed to GET %q: %v", url, err)
	}
	if resp.StatusCode != 200 {
		return body, fmt.Errorf("GET %q got status code %d, want 200", url, resp.StatusCode)
	}
	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return body, fmt.Errorf("failed to read response from GET %q : %v", url, err)
	}
	err = json.Unmarshal(bytes, &body)
	if err != nil {
		return body, fmt.Errorf("failed to marshal response body %s from %q into ResponseBody of echo server: %v", bytes, url, err)
	}
	return body, nil
}

// CheckDeployment checks if the given deployment is in a stable state.
func CheckDeployment(deployment *apps.Deployment) error {
	if deployment.Spec.Replicas == nil {
		return fmt.Errorf("deployment %s/%s has nil replicas: %v", deployment.Namespace, deployment.Name, deployment)
	}

	wantedReplicas := *deployment.Spec.Replicas

	for _, f := range []struct {
		v    *int32
		name string
		want int32
	}{
		{&deployment.Status.Replicas, "replicas", wantedReplicas},
		{&deployment.Status.ReadyReplicas, "ready replicas", wantedReplicas},
		{&deployment.Status.AvailableReplicas, "available replicas", wantedReplicas},
		{&deployment.Status.UpdatedReplicas, "updated replicas", wantedReplicas},
		{&deployment.Status.UnavailableReplicas, "unavailable replicas", 0},
	} {
		if *f.v != f.want {
			return fmt.Errorf("deployment %s/%s has %d %s, want %d", deployment.Namespace, deployment.Name, *f.v, f.name, f.want)
		}
	}
	return nil
}

// CheckNegs checks if the network endpoints in the NEGs is in expected state
func CheckNegs(negs map[meta.Key]*fuzz.NetworkEndpoints, expectHealthy bool, expectCount int) error {
	var (
		count    int
		errs     []error
		negNames []string
	)
	for key, neg := range negs {
		count += len(neg.Endpoints)
		negNames = append(negNames, key.String())

		if expectHealthy {
			for _, ep := range neg.Endpoints {
				json, _ := ep.NetworkEndpoint.MarshalJSON()
				if ep.Healths == nil || len(ep.Healths) != 1 || ep.Healths[0] == nil {
					errs = append(errs, fmt.Errorf("network endpoint %s in NEG %v has health status %v, want 1 health status", json, key.String(), ep.Healths))
					continue
				}

				health := ep.Healths[0].HealthState
				if health != healthyState {
					errs = append(errs, fmt.Errorf("network endpoint %s in NEG %v has health status %q, want %q", json, key.String(), health, healthyState))
				}
			}
		}
	}

	if count != expectCount {
		return fmt.Errorf("NEGs (%v) have a total %v of endpoints, want %v", strings.Join(negNames, "/"), count, expectCount)
	}

	return utilerrors.NewAggregate(errs)
}

// CheckNegStatus checks if the NEG Status annotation is presented and in the expected state
func CheckNegStatus(svc *v1.Service, expectSvcPors []string) (annotations.NegStatus, error) {
	annotation, ok := svc.Annotations[annotations.NEGStatusKey]
	if !ok {
		return annotations.NegStatus{}, fmt.Errorf("service %s/%s does not have neg status annotation: %v", svc.Namespace, svc.Name, svc)
	}

	negStatus, err := annotations.ParseNegStatus(annotation)
	if err != nil {
		return negStatus, fmt.Errorf("service %s/%s has invalid neg status annotation %q: %v", svc.Namespace, svc.Name, annotation, err)
	}

	expectPorts := sets.NewString(expectSvcPors...)
	existingPorts := sets.NewString()
	for port := range negStatus.NetworkEndpointGroups {
		existingPorts.Insert(port)
	}

	if !expectPorts.Equal(existingPorts) {
		return negStatus, fmt.Errorf("service %s/%s does not have neg status annotation: %q, want ports %q", svc.Namespace, svc.Name, annotation, expectPorts.List())
	}
	return negStatus, nil
}
