// Copyright 2019 The Cluster Monitoring Operator Authors
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

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osConfigv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-monitoring-operator/test/e2e/framework"
)

func TestPrometheusMetrics(t *testing.T) {
	for service, expected := range map[string]int{
		"prometheus-operator":           1,
		"prometheus-k8s":                2,
		"prometheus-k8s-thanos-sidecar": 2,
		"thanos-querier":                2,
		"prometheus-adapter":            2,
		"alertmanager-main":             2,
		"kube-state-metrics":            2, // one for the kube metrics + one for the metrics of the process itself.
		"openshift-state-metrics":       2, // ditto.
		"telemeter-client":              1,
	} {
		t.Run(service, func(t *testing.T) {
			f.ThanosQuerierClient.WaitForQueryReturn(
				t, 10*time.Minute, fmt.Sprintf(`count(up{service="%s",namespace="openshift-monitoring"} == 1)`, service),
				func(i int) error {
					if i != expected {
						return fmt.Errorf("expected %d targets to be up but got %d", expected, i)
					}

					return nil
				},
			)
		})
	}
}

func TestAntiAffinity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		instance string
	}{
		{
			name:     "alertmanager",
			instance: "main",
		},
		{
			name:     "prometheus",
			instance: "k8s",
		},
	} {
		t.Run(fmt.Sprintf("name=%q", tc.name), func(t *testing.T) {
			ctx := context.Background()
			pods, err := f.KubeClient.CoreV1().Pods(f.Ns).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/name=%s", tc.instance, tc.name),
				FieldSelector: "status.phase=Running",
			})
			if err != nil {
				t.Fatal(err)
			}

			if len(pods.Items) != 2 {
				t.Fatalf("expecting 2 pods, got %d", len(pods.Items))
			}

			pod := pods.Items[0]
			if pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
				t.Fatal("pod doesn't define requiredDuringSchedulingIgnoredDuringExecution")
			}
		})
	}
}

type remoteWriteTest struct {
	query       string
	expected    func(int) bool
	description string
}

func TestPrometheusRemoteWrite(t *testing.T) {
	ctx := context.Background()

	name := "rwe2e"

	// deploy a service for our remote write target
	svc := f.MakePrometheusService(f.Ns, name, name, v1.ServiceTypeClusterIP)

	if err := f.OperatorClient.CreateOrUpdateService(ctx, svc); err != nil {
		t.Fatal(err)
	}
	prometheusReceiverURL := svc.Name + "." + svc.Namespace + ".svc.cluster.local"

	// setup a self-signed ca and store the artifacts in a secret
	secName := fmt.Sprintf("selfsigned-%s-bundle", name)
	tlsSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secName,
			Namespace: f.Ns,
			Labels: map[string]string{
				"group":                    name,
				framework.E2eTestLabelName: framework.E2eTestLabelValue,
			},
		},
		Data: map[string][]byte{
			"client-cert-name": []byte("remoteWrite-client"),
			"serving-cert-url": []byte(prometheusReceiverURL),
		},
	}
	if err := createSelfSignedMTLSArtifacts(tlsSecret); err != nil {
		t.Fatal(err)
	}
	if err := f.OperatorClient.CreateIfNotExistSecret(ctx, tlsSecret); err != nil {
		t.Fatal(err)
	}

	route := f.MakePrometheusServiceRoute(svc)
	if err := f.OperatorClient.CreateRouteIfNotExists(ctx, route); err != nil {
		t.Fatal(err)
	}

	prometheusReceiveClient, err := framework.NewPrometheusClientFromRoute(
		ctx,
		f.OpenShiftRouteClient,
		route.Namespace,
		route.Name,
		"")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		rwSpec   string
		expected []remoteWriteTest
	}{
		{
			name: "assert remote write without authorization works",
			rwSpec: `
  - url: https://%[1]s/api/v1/write
    tlsConfig:
      ca:
        secret:
          name: %[2]s
          key: ca.crt`,
			expected: []remoteWriteTest{
				{
					query:       fmt.Sprintf(`ceil(delta(prometheus_remote_storage_samples_pending{pod="%[1]s",prometheus_replica="%[1]s"}[1m]))`, "prometheus-k8s-0"),
					expected:    func(v int) bool { return v != 0 },
					description: "prometheus_remote_storage_samples_pending indicates no remote write progress, expected a continuously changing delta",
				},
				{
					query:       fmt.Sprintf(`ceil(delta(prometheus_remote_storage_samples_pending{pod="%[1]s",prometheus_replica="%[1]s"}[1m]))`, "prometheus-k8s-1"),
					expected:    func(v int) bool { return v != 0 },
					description: "prometheus_remote_storage_samples_pending indicates no remote write progress, expected a continuously changing delta",
				},
			},
		},
		{
			name: "assert remote write with mtls authorization works",
			rwSpec: `
  - url: https://%[1]s/api/v1/write
    tlsConfig:
      ca:
        secret:
          name: %[2]s
          key: ca.crt
      cert:
        secret:
          name: %[2]s
          key: client.crt
      keySecret:
        name: %[2]s
        key: client.key
`,
			expected: []remoteWriteTest{
				{
					query:       fmt.Sprintf(`ceil(delta(prometheus_remote_storage_samples_pending{pod="%[1]s",prometheus_replica="%[1]s"}[1m]))`, "prometheus-k8s-0"),
					expected:    func(v int) bool { return v != 0 },
					description: "prometheus_remote_storage_samples_pending indicates no remote write progress, expected a continuously changing delta",
				},
				{
					query:       fmt.Sprintf(`ceil(delta(prometheus_remote_storage_samples_pending{pod="%[1]s",prometheus_replica="%[1]s"}[1m]))`, "prometheus-k8s-1"),
					expected:    func(v int) bool { return v != 0 },
					description: "prometheus_remote_storage_samples_pending indicates no remote write progress, expected a continuously changing delta",
				},
			},
		},
		{
			name: "assert remote write temporary cluster id label is dropped",
			rwSpec: `
  - url: https://%[1]s/api/v1/write
    tlsConfig:
      ca:
        secret:
          name: %[2]s
          key: ca.crt
`,
			expected: []remoteWriteTest{
				{
					query:       `absent(prometheus_remote_storage_samples_pending{__tmp_openshift_cluster_id__=~".+"})`,
					expected:    func(v int) bool { return v == 1 },
					description: "Expected to find 0 time series of metric prometheus_remote_storage_samples_pending with the temporary cluster_id label",
				},
			},
		},
		{
			name: "assert remote write cluster_id relabel config works",
			rwSpec: `
  - url: https://%[1]s/api/v1/write
    tlsConfig:
      ca:
        secret:
          name: %[2]s
          key: ca.crt
    writeRelabelConfigs:
    - sourceLabels:
        - __tmp_openshift_cluster_id__
      targetLabel: cluster_id
      action: replace
`,
			expected: []remoteWriteTest{
				{
					query:       `count(prometheus_remote_storage_samples_pending{cluster_id!=""})`,
					expected:    func(v int) bool { return v == 4 },
					description: "Expected to find 4 time series of metric prometheus_remote_storage_samples_pending with the cluster_id label",
				},
				{
					query:       `absent(prometheus_remote_storage_samples_pending{__tmp_openshift_cluster_id__=~".+"})`,
					expected:    func(v int) bool { return v == 1 },
					description: "Expected to find 0 time series of metric prometheus_remote_storage_samples_pending with the temporary cluster_id label",
				},
			},
		},
	} {
		rw := fmt.Sprintf(tc.rwSpec, prometheusReceiverURL, tlsSecret.Name)

		cmoConfigMap := fmt.Sprintf(`prometheusK8s:
  logLevel: debug
  remoteWrite:%s
`, rw)

		t.Run(tc.name, func(t *testing.T) {
			// deploy remote write target
			prometheusReceiver := f.MakePrometheusWithWebTLSRemoteReceive(name, secName)
			if err := f.OperatorClient.CreateOrUpdatePrometheus(ctx, prometheusReceiver); err != nil {
				t.Fatal(err)
			}
			if err := f.OperatorClient.WaitForPrometheus(ctx, prometheusReceiver); err != nil {
				t.Fatal(err)
			}

			f.MustCreateOrUpdateConfigMap(t, configMapWithData(t, cmoConfigMap))

			f.AssertOperatorCondition(osConfigv1.OperatorDegraded, osConfigv1.ConditionFalse)(t)
			f.AssertOperatorCondition(osConfigv1.OperatorProgressing, osConfigv1.ConditionFalse)(t)
			f.AssertOperatorCondition(osConfigv1.OperatorAvailable, osConfigv1.ConditionTrue)(t)

			remoteWriteCheckMetrics(ctx, t, prometheusReceiveClient, tc.expected)

			if err := f.OperatorClient.DeletePrometheus(ctx, prometheusReceiver); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func remoteWriteCheckMetrics(ctx context.Context, t *testing.T, promClient *framework.PrometheusClient, tests []remoteWriteTest) {
	for _, test := range tests {
		promClient.WaitForQueryReturn(
			t, 2*time.Minute, test.query,
			func(v int) error {
				if !test.expected(v) {
					return fmt.Errorf(test.description)
				}
				return nil
			},
		)
	}
}
