// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package deploy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"time"

	dtypes "github.com/docker/docker/api/types"
	dclient "github.com/docker/docker/client"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/kind/pkg/cluster"
)

type ClusterSpec struct {
	Kind string    `yaml:"kind"`
	Spec yaml.Node `yaml:"spec"`
}

type IngressSpec struct {
	Kind string    `yaml:"kind"`
	Spec yaml.Node `yaml:"spec"`
}
type CNISpec struct {
	Kind string    `yaml:"kind"`
	Spec yaml.Node `yaml:"spec"`
}

type KindSpec struct {
	Name             string        `yaml:"name"`
	Recycle          bool          `yaml:"recycle"`
	Version          string        `yaml:"version"`
	Image            string        `yaml:"image"`
	Retain           bool          `yaml:"retain"`
	Wait             time.Duration `yaml:"wait"`
	Kubecfg          string        `yaml:"kubecfg"`
	DeployWithClient bool          `yaml:"deployWithClient"`
	Load bool `yaml:"load"`
	execer           func(string, ...string) error
}

//go:generate mockgen -source=specs.go -destination=mocks/mock_provider.go -package=mocks provider
type provider interface {
	List() ([]string, error)
	Create(name string, options ...cluster.CreateOption) error
}

var (
	// newProvider is the kind provider that is replacable for testing.
	newProvider = defaultProvider

	execLookPath = exec.LookPath
)

func defaultProvider() provider {
	return cluster.NewProvider(cluster.ProviderWithLogger(&logAdapter{log.StandardLogger()}))
}

func (k *KindSpec) Deploy(ctx context.Context) error {
	provider := newProvider()
	if k.Recycle {
		clusters, err := provider.List()
		if err != nil {
			return err
		}
		for _, v := range clusters {
			if k.Name == v {
				log.Infof("Recycling existing cluster: %s", v)
				return nil
			}
		}
	}
	if k.DeployWithClient {
		if err := provider.Create(
			k.Name,
			cluster.CreateWithNodeImage(k.Image),
			cluster.CreateWithRetain(k.Retain),
			cluster.CreateWithWaitForReady(k.Wait),
			cluster.CreateWithKubeconfigPath(k.Kubecfg),
			cluster.CreateWithDisplayUsage(true),
			cluster.CreateWithDisplaySalutation(true),
		); err != nil {
			return errors.Wrap(err, "failed to create cluster using kind client")
		}
		log.Infof("Deployed kind cluster using kind client: %s", k.Name)
		return nil
	}
	if k.execer == nil {
		k.execer = execCmd
	}
	if _, err := execLookPath("kind"); err != nil {
		return errors.Wrap(err, "install kind cli to deploy, or set the deployWithClient field")
	}
	args := []string{"create", "cluster"}
	if k.Name != "" {
		args = append(args, "--name", k.Name)
	}
	if k.Image != "" {
		args = append(args, "--image", k.Image)
	}
	if k.Retain {
		args = append(args, "--retain")
	}
	if k.Wait != 0 {
		args = append(args, "--wait", k.Wait.String())
	}
	if k.Kubecfg != "" {
		args = append(args, "--kubeconfig", k.Kubecfg)
	}
	if err := k.execer("kind", args...); err != nil {
		return errors.Wrap(err, "failed to create cluster using cli")
	}
	log.Infof("Deployed kind cluster: %s", k.Name)
	if !k.Load {
		return nil
	}
	loadArgs := []string{"load", "docker-image", "quay.io/metallb/controller:main", "quay.io/metallb/speaker:main", "networkop/meshnet", "networkop/init-wait"}
	if k.Name != "" {
		loadArgs = append(loadArgs, "--name", k.Name)
	}
	if err := k.execer("kind", loadArgs...); err != nil {
		return errors.Wrap(err, "failed to load docker images in cluster using cli")
	}
	return nil
}

type MetalLBSpec struct {
	Version     string `yaml:"version"`
	IPCount     int    `yaml:"ip_count"`
	ManifestDir string `yaml:"manifests"`
	kClient     kubernetes.Interface
	execer      func(string, ...string) error
	dClient     dclient.NetworkAPIClient
}

func (m *MetalLBSpec) SetKClient(c kubernetes.Interface) {
	m.kClient = c
}

func inc(ip net.IP, cnt int) {
	for cnt > 0 {
		for j := len(ip) - 1; j >= 0; j-- {
			ip[j]++
			if ip[j] > 0 {
				break
			}
		}
		cnt--
	}
}

type pool struct {
	Name      string   `yaml:"name"`
	Protocol  string   `yaml:"protocol"`
	Addresses []string `yaml:"addresses"`
}

type metalLBConfig struct {
	AddressPools []pool `yaml:"address-pools"`
}

func execCmd(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	c.Stderr = log.StandardLogger().Out
	c.Stdout = log.StandardLogger().Out
	log.Info(c.String())
	if err := c.Run(); err != nil {
		return fmt.Errorf("%q failed: %v", c.String(), err)
	}
	return nil
}
func makeConfig(n *net.IPNet, count int) metalLBConfig {
	start := make(net.IP, len(n.IP))
	copy(start, n.IP)
	inc(start, 50)
	end := make(net.IP, len(start))
	copy(end, start)
	inc(end, count)
	return metalLBConfig{
		AddressPools: []pool{{
			Name:      "default",
			Protocol:  "layer2",
			Addresses: []string{fmt.Sprintf("%s - %s", start, end)},
		}},
	}
}
func (m *MetalLBSpec) Deploy(ctx context.Context) error {
	if m.execer == nil {
		m.execer = execCmd
	}
	if m.dClient == nil {
		var err error
		m.dClient, err = dclient.NewClientWithOpts(dclient.FromEnv)
		if err != nil {
			return err
		}
	}
	mPath := filepath.Join(deploymentBasePath, m.ManifestDir)
	log.Infof("Deploying metallb from: %s", mPath)
	log.Infof("Creating metallb namespace")
	if err := m.execer("kubectl", "apply", "-f", filepath.Join(mPath, "namespace.yaml")); err != nil {
		return err
	}
	_, err := m.kClient.CoreV1().Secrets("metallb-system").Get(ctx, "memberlist", metav1.GetOptions{})
	if err != nil {
		log.Infof("Creating metallb secret")
		d := make([]byte, 16)
		rand.Read(d)
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "memberlist",
			},
			StringData: map[string]string{
				"secretkey": base64.StdEncoding.EncodeToString(d),
			},
		}
		_, err := m.kClient.CoreV1().Secrets("metallb-system").Create(ctx, s, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}
	log.Infof("Applying metallb pods")
	if err := m.execer("kubectl", "apply", "-f", filepath.Join(mPath, "metallb.yaml")); err != nil {
		return err
	}
	_, err = m.kClient.CoreV1().ConfigMaps("metallb-system").Get(ctx, "config", metav1.GetOptions{})
	if err != nil {
		log.Infof("Applying metallb ingress config")
		// Get Network information from docker.
		nr, err := m.dClient.NetworkList(ctx, dtypes.NetworkListOptions{})
		if err != nil {
			return err
		}
		var network dtypes.NetworkResource
		for _, v := range nr {
			if v.Name == "kind" {
				network = v
				break
			}
		}
		var n *net.IPNet
		for _, ipRange := range network.IPAM.Config {
			_, ipNet, err := net.ParseCIDR(ipRange.Subnet)
			if err != nil {
				return err
			}
			if ipNet.IP.To4() != nil {
				n = ipNet
				break
			}
		}
		if n == nil {
			return fmt.Errorf("failed to find kind ipv4 docker net")
		}
		config := makeConfig(n, m.IPCount)
		b, err := yaml.Marshal(config)
		if err != nil {
			return err
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "config",
			},
			Data: map[string]string{
				"config": string(b),
			},
		}
		_, err = m.kClient.CoreV1().ConfigMaps("metallb-system").Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *MetalLBSpec) Healthy(ctx context.Context) error {
	log.Infof("Waiting on Metallb to be Healthy")
	w, err := m.kClient.AppsV1().Deployments("metallb-system").Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	ch := w.ResultChan()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled before healthy")
		case e, ok := <-ch:
			if !ok {
				return fmt.Errorf("watch channel closed before healthy")
			}
			d, ok := e.Object.(*appsv1.Deployment)
			if !ok {
				return fmt.Errorf("invalid object type: %T", d)
			}
			if d.Status.AvailableReplicas == 1 &&
				d.Status.ReadyReplicas == 1 &&
				d.Status.UnavailableReplicas == 0 &&
				d.Status.Replicas == 1 &&
				d.Status.UpdatedReplicas == 1 {
				log.Infof("Metallb Healthy")
				return nil
			}
		}
	}
}

type MeshnetSpec struct {
	Image       string `yaml:"image"`
	ManifestDir string `yaml:"manifests"`
	kClient     kubernetes.Interface
	execer      func(string, ...string) error
}

func (m *MeshnetSpec) SetKClient(c kubernetes.Interface) {
	m.kClient = c
}

func (m *MeshnetSpec) Deploy(ctx context.Context) error {
	if m.execer == nil {
		m.execer = execCmd
	}
	mPath := filepath.Join(deploymentBasePath, m.ManifestDir)
	log.Infof("Deploying Meshnet from: %s", mPath)
	if err := m.execer("kubectl", "apply", "-k", mPath); err != nil {
		return err
	}
	log.Infof("Meshnet Deployed")
	return nil
}

func (m *MeshnetSpec) Healthy(ctx context.Context) error {
	log.Infof("Waiting on Meshnet to be Healthy")
	w, err := m.kClient.AppsV1().DaemonSets("meshnet").Watch(ctx, metav1.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{metav1.ObjectNameField: "meshnet"}).String(),
	})
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled before healthy")
		case e, ok := <-w.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed before healthy")
			}
			d, ok := e.Object.(*appsv1.DaemonSet)
			if !ok {
				return fmt.Errorf("invalid object type: %T", d)
			}
			if d.Status.NumberReady == d.Status.DesiredNumberScheduled &&
				d.Status.NumberUnavailable == 0 {
				log.Infof("Meshnet Healthy")
				return nil
			}
		}
	}
}
