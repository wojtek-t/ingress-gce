/*
Copyright 2017 The Kubernetes Authors.

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

package types

import (
	"fmt"
	"reflect"
	"testing"

	istioV1alpha3 "istio.io/api/networking/v1alpha3"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/ingress-gce/pkg/annotations"
)

type negNamer struct{}

func (*negNamer) NEG(namespace, name string, svcPort int32) string {
	return fmt.Sprintf("%v-%v-%v", namespace, name, svcPort)
}

func (*negNamer) NEGWithSubset(namespace, name, subset string, svcPort int32) string {
	return fmt.Sprintf("%v-%v-%v-%v", namespace, name, subset, svcPort)
}

func (*negNamer) IsNEG(name string) bool {
	return false
}

func createDestinationRule(host string, subsets ...string) *istioV1alpha3.DestinationRule {
	ds := istioV1alpha3.DestinationRule{
		Host: host,
	}
	for _, subset := range subsets {
		ds.Subsets = append(ds.Subsets, &istioV1alpha3.Subset{Name: subset})
	}
	return &ds
}

func TestPortInfoMapMerge(t *testing.T) {
	namer := &negNamer{}
	namespace := "namespace"
	name := "name"
	testcases := []struct {
		desc        string
		p1          PortInfoMap
		p2          PortInfoMap
		expectedMap PortInfoMap
		expectErr   bool
	}{
		{
			"empty map union empty map",
			PortInfoMap{},
			PortInfoMap{},
			PortInfoMap{},
			false,
		},
		{
			"empty map union a non-empty map is the non-empty map",
			PortInfoMap{},
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, false),
			false,
		},
		{
			"empty map union a non-empty map is the non-empty map 2",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, true),
			PortInfoMap{},
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, true),
			false,
		},
		{
			"union of two non-empty maps, none has readiness gate enabled",
			NewPortInfoMap(namespace, name, SvcPortMap{443: "3000", 5000: "6000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 8080: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000", 5000: "6000", 8080: "9000"}, namer, false),
			false,
		},
		{
			"union of two non-empty maps, all have readiness gate enabled ",
			NewPortInfoMap(namespace, name, SvcPortMap{443: "3000", 5000: "6000"}, namer, true),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 8080: "9000"}, namer, true),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000", 5000: "6000", 8080: "9000"}, namer, true),
			false,
		},
		{
			"union of two non-empty maps with one overlapping service port",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "3000", 5000: "6000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "3000", 8080: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "3000", 5000: "6000", 8080: "9000"}, namer, false),
			false,
		},
		{
			"union of two non-empty maps with overlapping service port and difference in readiness gate configurations ",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "3000", 5000: "6000"}, namer, true),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "3000", 8080: "9000"}, namer, false),
			PortInfoMap{
				PortInfoMapKey{80, ""}: PortInfo{
					TargetPort:    "3000",
					NegName:       namer.NEG(namespace, name, 80),
					ReadinessGate: true,
				},
				PortInfoMapKey{5000, ""}: PortInfo{
					TargetPort:    "6000",
					NegName:       namer.NEG(namespace, name, 5000),
					ReadinessGate: true,
				},
				PortInfoMapKey{8080, ""}: PortInfo{
					TargetPort:    "9000",
					NegName:       namer.NEG(namespace, name, 8080),
					ReadinessGate: false,
				},
			},
			false,
		},
		{
			"union of two non-empty maps with overlapping service port and difference in readiness gate configurations ",
			helperNewPortInfoMapWithDestinationRule(namespace, name, SvcPortMap{80: "3000"}, namer, true,
				createDestinationRule(name, "v1", "v2")),
			helperNewPortInfoMapWithDestinationRule(namespace, name, SvcPortMap{80: "3000", 8080: "9000"}, namer, false,
				createDestinationRule(name, "v3")),
			PortInfoMap{
				PortInfoMapKey{80, "v1"}: PortInfo{
					TargetPort:    "3000",
					Subset:        "v1",
					NegName:       namer.NEGWithSubset(namespace, name, "v1", 80),
					ReadinessGate: true,
				},
				PortInfoMapKey{80, "v2"}: PortInfo{
					TargetPort:    "3000",
					Subset:        "v2",
					NegName:       namer.NEGWithSubset(namespace, name, "v2", 80),
					ReadinessGate: true,
				},
				PortInfoMapKey{80, "v3"}: PortInfo{
					TargetPort:    "3000",
					Subset:        "v3",
					NegName:       namer.NEGWithSubset(namespace, name, "v3", 80),
					ReadinessGate: false,
				},
				PortInfoMapKey{8080, "v3"}: PortInfo{
					TargetPort:    "9000",
					Subset:        "v3",
					NegName:       namer.NEGWithSubset(namespace, name, "v3", 8080),
					ReadinessGate: false,
				},
			},
			false,
		},
		{
			"error on inconsistent value",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "3000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 8000: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000", 5000: "6000", 8080: "9000"}, namer, false),
			true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			err := tc.p1.Merge(tc.p2)
			if tc.expectErr && err == nil {
				t.Errorf("Expect error != nil, got %v", err)
			}

			if !tc.expectErr && err != nil {
				t.Errorf("Expect error == nil, got %v", err)
			}

			if !tc.expectErr {
				if !reflect.DeepEqual(tc.p1, tc.expectedMap) {
					t.Errorf("Expected p1.Merge(p2) to equal: %v; got: %v", tc.expectedMap, tc.p1)
				}
			}
		})
	}
}

func helperNewPortInfoMapWithDestinationRule(namespace, name string, svcPortMap SvcPortMap, namer NetworkEndpointGroupNamer, readinessGate bool,
	destinationRule *istioV1alpha3.DestinationRule) PortInfoMap {
	rsl, _ := NewPortInfoMapWithDestinationRule(namespace, name, svcPortMap, namer, readinessGate, destinationRule)
	return rsl
}

func TestPortInfoMapDifference(t *testing.T) {
	namer := &negNamer{}
	namespace := "namespace"
	name := "name"
	testcases := []struct {
		desc        string
		p1          PortInfoMap
		p2          PortInfoMap
		expectedMap PortInfoMap
	}{
		{
			"empty map difference empty map",
			PortInfoMap{},
			PortInfoMap{},
			PortInfoMap{},
		},
		{
			"empty map difference a non-empty map is empty map",
			PortInfoMap{},
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, false),
			PortInfoMap{},
		},
		{
			"non-empty map difference a non-empty map is the non-empty map",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, false),
			PortInfoMap{},
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, false),
		},
		{
			"non-empty map difference a non-empty map is the non-empty map 2",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, true),
			PortInfoMap{},
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, true),
		},
		{
			"difference of two non-empty maps with the same elements",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, false),
			PortInfoMap{},
		},
		{
			"difference of two non-empty maps with no elements in common returns p1",
			NewPortInfoMap(namespace, name, SvcPortMap{443: "3000", 5000: "6000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 8080: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{443: "3000", 5000: "6000"}, namer, false),
		},
		{
			"difference of two non-empty maps with elements in common",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000", 5000: "6000", 8080: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 8080: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{443: "3000", 5000: "6000"}, namer, false),
		},
		{
			"difference of two non-empty maps with a key in common but different in value",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "8080", 8080: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport"}, namer, false),
		},
		{
			"difference of two non-empty maps with 2 keys in common but different in values",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "8443"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "8080", 443: "9443"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "8443"}, namer, false),
		},
		{
			"difference of two non-empty maps with a key in common but different in readiness gate fields",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "8080"}, namer, true),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "8080", 8080: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "8080"}, namer, true),
		},
		{
			"difference of two non-empty maps with 2 keys in common and 2 more items with different readinessGate",
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000", 5000: "6000", 8080: "9000"}, namer, true),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 8080: "9000"}, namer, false),
			NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000", 5000: "6000", 8080: "9000"}, namer, true),
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			result := tc.p1.Difference(tc.p2)
			if !reflect.DeepEqual(result, tc.expectedMap) {
				t.Errorf("Expected p1.Difference(p2) to equal: %v; got: %v", tc.expectedMap, result)
			}
		})
	}
}

func TestPortInfoMapToPortNegMap(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc             string
		portInfoMap      PortInfoMap
		expectPortNegMap annotations.PortNegMap
	}{
		{
			desc:             "Test empty struct",
			portInfoMap:      PortInfoMap{},
			expectPortNegMap: annotations.PortNegMap{},
		},
		{
			desc:             "1 port",
			portInfoMap:      PortInfoMap{PortInfoMapKey{80, ""}: PortInfo{NegName: "neg1"}},
			expectPortNegMap: annotations.PortNegMap{"80": "neg1"},
		},
		{
			desc:             "2 ports",
			portInfoMap:      PortInfoMap{PortInfoMapKey{80, ""}: PortInfo{NegName: "neg1"}, PortInfoMapKey{8080, ""}: PortInfo{NegName: "neg2"}},
			expectPortNegMap: annotations.PortNegMap{"80": "neg1", "8080": "neg2"},
		},
		{
			desc:             "3 ports",
			portInfoMap:      PortInfoMap{PortInfoMapKey{80, ""}: PortInfo{NegName: "neg1"}, PortInfoMapKey{443, ""}: PortInfo{NegName: "neg2"}, PortInfoMapKey{8080, ""}: PortInfo{NegName: "neg3"}},
			expectPortNegMap: annotations.PortNegMap{"80": "neg1", "443": "neg2", "8080": "neg3"},
		},
	} {
		res := tc.portInfoMap.ToPortNegMap()
		if !reflect.DeepEqual(res, tc.expectPortNegMap) {
			t.Errorf("For test case %q, expect %v, but got %v", tc.desc, tc.expectPortNegMap, res)
		}
	}
}

func TestNegsWithReadinessGate(t *testing.T) {
	t.Parallel()

	namer := &negNamer{}
	namespace := "namespace"
	name := "name"
	for _, tc := range []struct {
		desc           string
		getPortInfoMap func() PortInfoMap
		expectNegs     sets.String
	}{
		{
			desc:           "empty PortInfoMap",
			getPortInfoMap: func() PortInfoMap { return PortInfoMap{} },
			expectNegs:     sets.NewString(),
		},
		{
			desc: "PortInfoMap with no readiness gate enabled",
			getPortInfoMap: func() PortInfoMap {
				return NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000", 5000: "6000", 8080: "9000"}, namer, false)
			},
			expectNegs: sets.NewString(),
		},
		{
			desc: "PortInfoMap with all readiness gates enabled",
			getPortInfoMap: func() PortInfoMap {
				return NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000", 5000: "6000", 8080: "9000"}, namer, true)
			},
			expectNegs: sets.NewString(namer.NEG(namespace, name, 80), namer.NEG(namespace, name, 443), namer.NEG(namespace, name, 5000), namer.NEG(namespace, name, 8080)),
		},
		{
			desc: "PortInfoMap with part of readiness gates enabled",
			getPortInfoMap: func() PortInfoMap {
				p := NewPortInfoMap(namespace, name, SvcPortMap{5000: "6000", 8080: "9000"}, namer, true)
				p.Merge(NewPortInfoMap(namespace, name, SvcPortMap{80: "namedport", 443: "3000"}, namer, false))
				return p
			},
			expectNegs: sets.NewString(namer.NEG(namespace, name, 5000), namer.NEG(namespace, name, 8080)),
		},
	} {
		negs := tc.getPortInfoMap().NegsWithReadinessGate()
		if !negs.Equal(tc.expectNegs) {
			t.Errorf("For test case %q, expect %v, but got %v", tc.desc, tc.expectNegs, negs)
		}
	}
}
