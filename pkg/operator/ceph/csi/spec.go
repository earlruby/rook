/*
Copyright 2019 The Rook Authors. All rights reserved.

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

package csi

import (
	"context"
	_ "embed"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkg/errors"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8scsi "k8s.io/api/storage/v1beta1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	cephcsi "github.com/ceph/ceph-csi/api/deploy/kubernetes"
)

type Param struct {
	CSIPluginImage                           string
	RegistrarImage                           string
	ProvisionerImage                         string
	AttacherImage                            string
	SnapshotterImage                         string
	ResizerImage                             string
	DriverNamePrefix                         string
	KubeletDirPath                           string
	CsiLogRootPath                           string
	ForceCephFSKernelClient                  string
	CephFSKernelMountOptions                 string
	CephFSPluginUpdateStrategy               string
	CephFSPluginUpdateStrategyMaxUnavailable string
	NFSPluginUpdateStrategy                  string
	RBDPluginUpdateStrategy                  string
	RBDPluginUpdateStrategyMaxUnavailable    string
	PluginPriorityClassName                  string
	ProvisionerPriorityClassName             string
	VolumeReplicationImage                   string
	CSIAddonsImage                           string
	ImagePullPolicy                          string
	CSIClusterName                           string
	CSIDomainLabels                          string
	GRPCTimeout                              time.Duration
	CSIEnableMetadata                        bool
	EnablePluginSelinuxHostMount             bool
	EnableCSIHostNetwork                     bool
	EnableOMAPGenerator                      bool
	EnableRBDSnapshotter                     bool
	EnableCephFSSnapshotter                  bool
	EnableNFSSnapshotter                     bool
	EnableCSIAddonsSideCar                   bool
	MountCustomCephConf                      bool
	EnableCSIDriverSeLinuxMount              bool
	EnableCSIEncryption                      bool
	EnableCSITopology                        bool
	EnableLiveness                           bool
	CephFSAttachRequired                     bool
	RBDAttachRequired                        bool
	NFSAttachRequired                        bool
	VolumeGroupSnapshotSupported             bool
	EnableVolumeGroupSnapshot                bool
	LogLevel                                 uint8
	SidecarLogLevel                          uint8
	CephFSLivenessMetricsPort                uint16
	CSIAddonsPort                            uint16
	RBDLivenessMetricsPort                   uint16
	KubeApiBurst                             uint16
	KubeApiQPS                               float32
	LeaderElectionLeaseDuration              time.Duration
	LeaderElectionRenewDeadline              time.Duration
	LeaderElectionRetryPeriod                time.Duration
	ProvisionerReplicas                      int32
	CSICephFSPodLabels                       map[string]string
	CSINFSPodLabels                          map[string]string
	CSIRBDPodLabels                          map[string]string
	CSILogRotation                           bool
	CsiComponentName                         string
	CSILogRotationMaxSize                    string
	CSILogRotationPeriod                     string
	Privileged                               bool
}

type templateParam struct {
	Param
	// non-global template only parameters
	Namespace string
}

type driverDetails struct {
	name           string
	fullName       string
	holderTemplate string
	toleration     string
	nodeAffinity   string
	resource       string
}

var (
	CSIParam Param

	EnableRBD                 = false
	EnableCephFS              = false
	EnableNFS                 = false
	enableCSIOperator         = false
	CustomCSICephConfigExists = false

	// driver names
	CephFSDriverName string
	NFSDriverName    string
	RBDDriverName    string

	// configuration map for csi
	ConfigName = "rook-ceph-csi-config"
	ConfigKey  = "csi-cluster-config-json"
)

// Specify default images as var instead of const so that they can be overridden with the Go
// linker's -X flag. This allows users to easily build images with a different opinionated set of
// images without having to specify them manually in charts/manifests which can make upgrades more
// manually challenging.
var (
	// image names
	DefaultCSIPluginImage   = "quay.io/cephcsi/cephcsi:v3.12.2"
	DefaultRegistrarImage   = "registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.11.1"
	DefaultProvisionerImage = "registry.k8s.io/sig-storage/csi-provisioner:v5.0.1"
	DefaultAttacherImage    = "registry.k8s.io/sig-storage/csi-attacher:v4.6.1"
	DefaultSnapshotterImage = "registry.k8s.io/sig-storage/csi-snapshotter:v8.0.1"
	DefaultResizerImage     = "registry.k8s.io/sig-storage/csi-resizer:v1.11.1"
	DefaultCSIAddonsImage   = "quay.io/csiaddons/k8s-sidecar:v0.9.1"

	// image pull policy
	DefaultCSIImagePullPolicy = string(corev1.PullIfNotPresent)

	// Local package template path for RBD
	//go:embed template/rbd/csi-rbdplugin.yaml
	RBDPluginTemplatePath string
	//go:embed template/rbd/csi-rbdplugin-holder.yaml
	RBDPluginHolderTemplatePath string
	//go:embed template/rbd/csi-rbdplugin-provisioner-dep.yaml
	RBDProvisionerDepTemplatePath string
	//go:embed template/rbd/csi-rbdplugin-svc.yaml
	RBDPluginServiceTemplatePath string

	// Local package template path for CephFS
	//go:embed template/cephfs/csi-cephfsplugin.yaml
	CephFSPluginTemplatePath string
	//go:embed template/cephfs/csi-cephfsplugin-holder.yaml
	CephFSPluginHolderTemplatePath string
	//go:embed template/cephfs/csi-cephfsplugin-provisioner-dep.yaml
	CephFSProvisionerDepTemplatePath string
	//go:embed template/cephfs/csi-cephfsplugin-svc.yaml
	CephFSPluginServiceTemplatePath string

	// Local package template path for NFS
	//go:embed template/nfs/csi-nfsplugin.yaml
	NFSPluginTemplatePath string
	//go:embed template/nfs/csi-nfsplugin-provisioner-dep.yaml
	NFSProvisionerDepTemplatePath string
	//go:embed template/nfs/csi-nfsplugin-holder.yaml
	NFSPluginHolderTemplatePath string

	//go:embed template/csi-logrotate-sidecar.yaml
	LogrotateTemplatePath string

	holderEnabled bool

	csiRootPath string
)

const (
	// common tolerations and node affinity
	provisionerTolerationsEnv  = "CSI_PROVISIONER_TOLERATIONS"
	provisionerNodeAffinityEnv = "CSI_PROVISIONER_NODE_AFFINITY"
	pluginTolerationsEnv       = "CSI_PLUGIN_TOLERATIONS"
	pluginNodeAffinityEnv      = "CSI_PLUGIN_NODE_AFFINITY"

	// CephFS tolerations and node affinity
	cephFSProvisionerTolerationsEnv  = "CSI_CEPHFS_PROVISIONER_TOLERATIONS"
	cephFSProvisionerNodeAffinityEnv = "CSI_CEPHFS_PROVISIONER_NODE_AFFINITY"
	cephFSPluginTolerationsEnv       = "CSI_CEPHFS_PLUGIN_TOLERATIONS"
	cephFSPluginNodeAffinityEnv      = "CSI_CEPHFS_PLUGIN_NODE_AFFINITY"

	// NFS tolerations and node affinity
	nfsProvisionerTolerationsEnv  = "CSI_NFS_PROVISIONER_TOLERATIONS"
	nfsProvisionerNodeAffinityEnv = "CSI_NFS_PROVISIONER_NODE_AFFINITY"
	nfsPluginTolerationsEnv       = "CSI_NFS_PLUGIN_TOLERATIONS"
	nfsPluginNodeAffinityEnv      = "CSI_NFS_PLUGIN_NODE_AFFINITY"

	// RBD tolerations and node affinity
	rbdProvisionerTolerationsEnv  = "CSI_RBD_PROVISIONER_TOLERATIONS"
	rbdProvisionerNodeAffinityEnv = "CSI_RBD_PROVISIONER_NODE_AFFINITY"
	rbdPluginTolerationsEnv       = "CSI_RBD_PLUGIN_TOLERATIONS"
	rbdPluginNodeAffinityEnv      = "CSI_RBD_PLUGIN_NODE_AFFINITY"

	// compute resource for CSI pods
	rbdProvisionerResource = "CSI_RBD_PROVISIONER_RESOURCE"
	rbdPluginResource      = "CSI_RBD_PLUGIN_RESOURCE"

	cephFSProvisionerResource = "CSI_CEPHFS_PROVISIONER_RESOURCE"
	cephFSPluginResource      = "CSI_CEPHFS_PLUGIN_RESOURCE"

	nfsProvisionerResource = "CSI_NFS_PROVISIONER_RESOURCE"
	nfsPluginResource      = "CSI_NFS_PLUGIN_RESOURCE"

	cephFSPluginVolume      = "CSI_CEPHFS_PLUGIN_VOLUME"
	cephFSPluginVolumeMount = "CSI_CEPHFS_PLUGIN_VOLUME_MOUNT"

	rbdPluginVolume      = "CSI_RBD_PLUGIN_VOLUME"
	rbdPluginVolumeMount = "CSI_RBD_PLUGIN_VOLUME_MOUNT"

	nfsPluginVolume      = "CSI_NFS_PLUGIN_VOLUME"
	nfsPluginVolumeMount = "CSI_NFS_PLUGIN_VOLUME_MOUNT"

	// kubelet directory path
	DefaultKubeletDirPath = "/var/lib/kubelet"

	// grpc metrics and liveness port for cephfs  and rbd
	DefaultCephFSGRPCMerticsPort     uint16 = 9091
	DefaultCephFSLivenessMerticsPort uint16 = 9081
	DefaultRBDGRPCMerticsPort        uint16 = 9090
	DefaultRBDLivenessMerticsPort    uint16 = 9080
	DefaultCSIAddonsPort             uint16 = 9070

	// default log level for csi containers
	defaultLogLevel        uint8 = 0
	defaultSidecarLogLevel uint8 = 0

	// default leader election flags
	defaultLeaderElectionLeaseDuration = 137 * time.Second
	defaultLeaderElectionRenewDeadline = 107 * time.Second
	defaultLeaderElectionRetryPeriod   = 26 * time.Second

	// GRPC timeout.
	defaultGRPCTimeout = 150
	grpcTimeout        = "CSI_GRPC_TIMEOUT_SECONDS"
	// default provisioner replicas
	defaultProvisionerReplicas int32 = 2

	// update strategy
	rollingUpdate = "RollingUpdate"
	onDelete      = "OnDelete"

	// driver daemonset names
	CsiRBDPlugin    = "csi-rbdplugin"
	CsiCephFSPlugin = "csi-cephfsplugin"
	CsiNFSPlugin    = "csi-nfsplugin"

	// driver deployment names
	csiRBDProvisioner    = "csi-rbdplugin-provisioner"
	csiCephFSProvisioner = "csi-cephfsplugin-provisioner"
	csiNFSProvisioner    = "csi-nfsplugin-provisioner"

	// cephcsi container names
	csiRBDContainerName    = "csi-rbdplugin"
	csiCephFSContainerName = "csi-cephfsplugin"
	csiNFSContainerName    = "csi-nfsplugin"

	RBDDriverShortName    = "rbd"
	CephFSDriverShortName = "cephfs"
	NFSDriverShortName    = "nfs"
	rbdDriverSuffix       = "rbd.csi.ceph.com"
	cephFSDriverSuffix    = "cephfs.csi.ceph.com"
	nfsDriverSuffix       = "nfs.csi.ceph.com"
	nodePlugin            = "node-plugin"
	controllerPlugin      = "controller-plugin"
)

func CSIEnabled() bool {
	return EnableRBD || EnableCephFS || EnableNFS
}

func IsHolderEnabled() bool {
	return holderEnabled
}

func validateCSIParam() error {
	if len(CSIParam.CSIPluginImage) == 0 {
		return errors.New("missing csi rbd plugin image")
	}
	if len(CSIParam.RegistrarImage) == 0 {
		return errors.New("missing csi registrar image")
	}
	if len(CSIParam.ProvisionerImage) == 0 {
		return errors.New("missing csi provisioner image")
	}
	if len(CSIParam.AttacherImage) == 0 {
		return errors.New("missing csi attacher image")
	}

	return nil
}

func (r *ReconcileCSI) startDrivers(ownerInfo *k8sutil.OwnerInfo) error {
	var (
		err                                                                             error
		rbdPlugin, cephfsPlugin, nfsPlugin                                              *apps.DaemonSet
		rbdProvisionerDeployment, cephfsProvisionerDeployment, nfsProvisionerDeployment *apps.Deployment
		rbdService, cephfsService                                                       *corev1.Service
		csiDriverobj                                                                    v1CsiDriver
	)

	enabledDrivers := make([]driverDetails, 0)

	tp := templateParam{
		Param:     CSIParam,
		Namespace: r.opConfig.OperatorNamespace,
	}

	if strings.HasSuffix(tp.DriverNamePrefix, ".") {
		// As operator is adding a dot at the end of the prefix, we should not
		// allow the user to add a dot at the end of the prefix. as it will
		// result in two dots at the end of the prefix. which cases the csi
		// driver name creation failure
		return errors.Errorf("driver name prefix %q should not end with a dot", tp.DriverNamePrefix)
	}

	err = validateCSIDriverNamePrefix(r.opManagerContext, r.context.Clientset, r.opConfig.OperatorNamespace, tp.DriverNamePrefix)
	if err != nil {
		return err
	}
	// Add a dot at the end of the prefix for having the driver name prefix
	// with format <prefix>.<driver-name>
	tp.DriverNamePrefix = fmt.Sprintf("%s.", tp.DriverNamePrefix)

	CephFSDriverName = tp.DriverNamePrefix + cephFSDriverSuffix
	RBDDriverName = tp.DriverNamePrefix + rbdDriverSuffix
	NFSDriverName = tp.DriverNamePrefix + nfsDriverSuffix

	tp.Param.MountCustomCephConf = CustomCSICephConfigExists

	if EnableRBD {
		tp.CsiComponentName = nodePlugin
		tp.CsiLogRootPath = path.Join(csiRootPath, RBDDriverName)
		rbdPlugin, err = templateToDaemonSet("rbdplugin", RBDPluginTemplatePath, tp)
		if err != nil {
			return errors.Wrap(err, "failed to load rbdplugin template")
		}
		rbdPlugin.Spec.RevisionHistoryLimit = opcontroller.RevisionHistoryLimit()
		if tp.CSILogRotation {
			applyLogrotateSidecar(&rbdPlugin.Spec.Template, "csi-rbd-daemonset-log-collector", LogrotateTemplatePath, tp)
		}

		tp.CsiComponentName = controllerPlugin
		rbdProvisionerDeployment, err = templateToDeployment("rbd-provisioner", RBDProvisionerDepTemplatePath, tp)
		if err != nil {
			return errors.Wrap(err, "failed to load rbd provisioner deployment template")
		}
		if tp.CSILogRotation {
			applyLogrotateSidecar(&rbdProvisionerDeployment.Spec.Template, "csi-rbd-deployment-log-collector", LogrotateTemplatePath, tp)
		}
		rbdProvisionerDeployment.Spec.Template.Spec.HostNetwork = opcontroller.EnforceHostNetwork()
		rbdProvisionerDeployment.Spec.RevisionHistoryLimit = opcontroller.RevisionHistoryLimit()

		// Create service if either liveness or GRPC metrics are enabled.
		if CSIParam.EnableLiveness {
			rbdService, err = templateToService("rbd-service", RBDPluginServiceTemplatePath, tp)
			if err != nil {
				return errors.Wrap(err, "failed to load rbd plugin service template")
			}
			rbdService.Namespace = r.opConfig.OperatorNamespace
		}
		enabledDrivers = append(enabledDrivers, driverDetails{
			name:           RBDDriverShortName,
			fullName:       RBDDriverName,
			holderTemplate: RBDPluginHolderTemplatePath,
			nodeAffinity:   rbdPluginNodeAffinityEnv,
			toleration:     rbdPluginTolerationsEnv,
			resource:       rbdPluginResource,
		})
	}
	if EnableCephFS {
		tp.CsiComponentName = nodePlugin
		tp.CsiLogRootPath = path.Join(csiRootPath, CephFSDriverName)
		cephfsPlugin, err = templateToDaemonSet("cephfsplugin", CephFSPluginTemplatePath, tp)
		if err != nil {
			return errors.Wrap(err, "failed to load CephFS plugin template")
		}
		cephfsPlugin.Spec.RevisionHistoryLimit = opcontroller.RevisionHistoryLimit()

		if tp.CSILogRotation {
			applyLogrotateSidecar(&cephfsPlugin.Spec.Template, "csi-cephfs-daemonset-log-collector", LogrotateTemplatePath, tp)
		}

		tp.CsiComponentName = controllerPlugin
		cephfsProvisionerDeployment, err = templateToDeployment("cephfs-provisioner", CephFSProvisionerDepTemplatePath, tp)
		if err != nil {
			return errors.Wrap(err, "failed to load rbd provisioner deployment template")
		}
		if tp.CSILogRotation {
			applyLogrotateSidecar(&cephfsProvisionerDeployment.Spec.Template, "csi-cephfs-deployment-log-collector", LogrotateTemplatePath, tp)
		}
		cephfsProvisionerDeployment.Spec.Template.Spec.HostNetwork = opcontroller.EnforceHostNetwork()
		cephfsProvisionerDeployment.Spec.RevisionHistoryLimit = opcontroller.RevisionHistoryLimit()

		// Create service if either liveness or GRPC metrics are enabled.
		if CSIParam.EnableLiveness {
			cephfsService, err = templateToService("cephfs-service", CephFSPluginServiceTemplatePath, tp)
			if err != nil {
				return errors.Wrap(err, "failed to load cephfs plugin service template")
			}
			cephfsService.Namespace = r.opConfig.OperatorNamespace
		}
		enabledDrivers = append(enabledDrivers, driverDetails{
			name:           CephFSDriverShortName,
			fullName:       CephFSDriverName,
			holderTemplate: CephFSPluginHolderTemplatePath,
			nodeAffinity:   cephFSPluginNodeAffinityEnv,
			toleration:     cephFSPluginTolerationsEnv,
			resource:       cephFSPluginResource,
		})
	}

	if EnableNFS {
		tp.CsiComponentName = nodePlugin
		tp.CsiLogRootPath = path.Join(csiRootPath, NFSDriverName)
		nfsPlugin, err = templateToDaemonSet("nfsplugin", NFSPluginTemplatePath, tp)
		if err != nil {
			return errors.Wrap(err, "failed to load nfs plugin template")
		}
		nfsPlugin.Spec.RevisionHistoryLimit = opcontroller.RevisionHistoryLimit()
		if tp.CSILogRotation {
			applyLogrotateSidecar(&nfsPlugin.Spec.Template, "csi-nfs-daemonset-log-collector", LogrotateTemplatePath, tp)
		}

		tp.CsiComponentName = controllerPlugin
		nfsProvisionerDeployment, err = templateToDeployment("nfs-provisioner", NFSProvisionerDepTemplatePath, tp)
		if err != nil {
			return errors.Wrap(err, "failed to load nfs provisioner deployment template")
		}
		if tp.CSILogRotation {
			applyLogrotateSidecar(&nfsProvisionerDeployment.Spec.Template, "csi-nfs-deployment-log-collector", LogrotateTemplatePath, tp)
		}
		nfsProvisionerDeployment.Spec.Template.Spec.HostNetwork = opcontroller.EnforceHostNetwork()
		nfsProvisionerDeployment.Spec.RevisionHistoryLimit = opcontroller.RevisionHistoryLimit()

		enabledDrivers = append(enabledDrivers, driverDetails{
			name:           NFSDriverShortName,
			fullName:       NFSDriverName,
			holderTemplate: NFSPluginHolderTemplatePath,
			nodeAffinity:   nfsPluginNodeAffinityEnv,
			toleration:     nfsPluginTolerationsEnv,
			resource:       nfsPluginResource,
		})
	}

	// get common provisioner tolerations and node affinity
	provisionerTolerations := getToleration(r.opConfig.Parameters, provisionerTolerationsEnv, []corev1.Toleration{})
	provisionerNodeAffinity := getNodeAffinity(r.opConfig.Parameters, provisionerNodeAffinityEnv, &corev1.NodeAffinity{})

	// get common plugin tolerations and node affinity
	pluginTolerations := getToleration(r.opConfig.Parameters, pluginTolerationsEnv, []corev1.Toleration{})
	pluginNodeAffinity := getNodeAffinity(r.opConfig.Parameters, pluginNodeAffinityEnv, &corev1.NodeAffinity{})

	// Deploy the CSI Holder DaemonSet if Multus is enabled or
	// EnableCSIHostNetwork is disabled.
	err = r.configureHolders(enabledDrivers, tp, pluginTolerations, pluginNodeAffinity)
	if err != nil {
		return errors.Wrap(err, "failed to configure holder")
	}

	if rbdPlugin != nil {
		// get RBD plugin tolerations and node affinity, defaults to common tolerations and node affinity if not specified
		rbdPluginTolerations := getToleration(r.opConfig.Parameters, rbdPluginTolerationsEnv, pluginTolerations)
		rbdPluginNodeAffinity := getNodeAffinity(r.opConfig.Parameters, rbdPluginNodeAffinityEnv, pluginNodeAffinity)
		// apply RBD plugin tolerations and node affinity
		applyToPodSpec(&rbdPlugin.Spec.Template.Spec, rbdPluginNodeAffinity, rbdPluginTolerations)
		// apply resource request and limit to rbdplugin containers
		applyResourcesToContainers(r.opConfig.Parameters, rbdPluginResource, &rbdPlugin.Spec.Template.Spec)
		// apply custom mounts to volumes
		applyVolumeToPodSpec(r.opConfig.Parameters, rbdPluginVolume, &rbdPlugin.Spec.Template.Spec)
		// apply custom mounts to volume mounts
		applyVolumeMountToContainer(r.opConfig.Parameters, rbdPluginVolumeMount, "csi-rbdplugin", &rbdPlugin.Spec.Template.Spec)
		err = ownerInfo.SetControllerReference(rbdPlugin)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to rbd plugin daemonset %q", rbdPlugin.Name)
		}
		err = r.applyCephClusterNetworkConfig(r.opManagerContext, &rbdPlugin.Spec.Template.ObjectMeta)
		if err != nil {
			return errors.Wrapf(err, "failed to apply network config to rbd plugin daemonset %q", rbdPlugin.Name)
		}
		if holderEnabled {
			rbdPlugin.Spec.Template.Spec.HostNetwork = false
		}
		err = k8sutil.CreateDaemonSet(r.opManagerContext, r.opConfig.OperatorNamespace, r.context.Clientset, rbdPlugin)
		if err != nil {
			return errors.Wrapf(err, "failed to start rbdplugin daemonset %q", rbdPlugin.Name)
		}
		k8sutil.AddRookVersionLabelToDaemonSet(rbdPlugin)
	}

	if rbdProvisionerDeployment != nil {
		// get RBD provisioner tolerations and node affinity, defaults to common tolerations and node affinity if not specified
		rbdProvisionerTolerations := getToleration(r.opConfig.Parameters, rbdProvisionerTolerationsEnv, provisionerTolerations)
		rbdProvisionerNodeAffinity := getNodeAffinity(r.opConfig.Parameters, rbdProvisionerNodeAffinityEnv, provisionerNodeAffinity)
		// apply RBD provisioner tolerations and node affinity
		applyToPodSpec(&rbdProvisionerDeployment.Spec.Template.Spec, rbdProvisionerNodeAffinity, rbdProvisionerTolerations)
		// apply resource request and limit to rbd provisioner containers
		applyResourcesToContainers(r.opConfig.Parameters, rbdProvisionerResource, &rbdProvisionerDeployment.Spec.Template.Spec)
		err = ownerInfo.SetControllerReference(rbdProvisionerDeployment)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to rbd provisioner deployment %q", rbdProvisionerDeployment.Name)
		}
		antiAffinity := GetPodAntiAffinity("app", csiRBDProvisioner)
		rbdProvisionerDeployment.Spec.Template.Spec.Affinity.PodAntiAffinity = &antiAffinity
		rbdProvisionerDeployment.Spec.Strategy = apps.DeploymentStrategy{
			Type: apps.RecreateDeploymentStrategyType,
		}

		err = r.applyCephClusterNetworkConfig(r.opManagerContext, &rbdProvisionerDeployment.Spec.Template.ObjectMeta)
		if err != nil {
			return errors.Wrapf(err, "failed to apply network config to rbd plugin provisioner deployment %q", rbdProvisionerDeployment.Name)
		}
		_, err = k8sutil.CreateOrUpdateDeployment(r.opManagerContext, r.context.Clientset, rbdProvisionerDeployment)
		if err != nil {
			return errors.Wrapf(err, "failed to start rbd provisioner deployment %q", rbdProvisionerDeployment.Name)
		}
		k8sutil.AddRookVersionLabelToDeployment(rbdProvisionerDeployment)
		logger.Info("successfully started CSI Ceph RBD driver")
	}

	if rbdService != nil {
		rbdService.Namespace = r.opConfig.OperatorNamespace
		err = ownerInfo.SetControllerReference(rbdService)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to rbd service %q", rbdService)
		}
		_, err = k8sutil.CreateOrUpdateService(r.opManagerContext, r.context.Clientset, r.opConfig.OperatorNamespace, rbdService)
		if err != nil {
			return errors.Wrapf(err, "failed to create rbd service %q", rbdService.Name)
		}
	}

	if cephfsPlugin != nil {
		// get CephFS plugin tolerations and node affinity, defaults to common tolerations and node affinity if not specified
		cephFSPluginTolerations := getToleration(r.opConfig.Parameters, cephFSPluginTolerationsEnv, pluginTolerations)
		cephFSPluginNodeAffinity := getNodeAffinity(r.opConfig.Parameters, cephFSPluginNodeAffinityEnv, pluginNodeAffinity)
		// apply CephFS plugin tolerations and node affinity
		applyToPodSpec(&cephfsPlugin.Spec.Template.Spec, cephFSPluginNodeAffinity, cephFSPluginTolerations)
		// apply resource request and limit to cephfs plugin containers
		applyResourcesToContainers(r.opConfig.Parameters, cephFSPluginResource, &cephfsPlugin.Spec.Template.Spec)
		// apply custom mounts to volumes
		applyVolumeToPodSpec(r.opConfig.Parameters, cephFSPluginVolume, &cephfsPlugin.Spec.Template.Spec)
		// apply custom mounts to volume mounts
		applyVolumeMountToContainer(r.opConfig.Parameters, cephFSPluginVolumeMount, "csi-cephfsplugin", &cephfsPlugin.Spec.Template.Spec)
		err = ownerInfo.SetControllerReference(cephfsPlugin)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to cephfs plugin daemonset %q", cephfsPlugin.Name)
		}
		err = r.applyCephClusterNetworkConfig(r.opManagerContext, &cephfsPlugin.Spec.Template.ObjectMeta)
		if err != nil {
			return errors.Wrapf(err, "failed to apply network config to cephfs plugin daemonset %q", cephfsPlugin.Name)
		}
		if holderEnabled {
			cephfsPlugin.Spec.Template.Spec.HostNetwork = false
			// HostPID is used to communicate with the network namespace
			cephfsPlugin.Spec.Template.Spec.HostPID = true
		}

		err = k8sutil.CreateDaemonSet(r.opManagerContext, r.opConfig.OperatorNamespace, r.context.Clientset, cephfsPlugin)
		if err != nil {
			return errors.Wrapf(err, "failed to start cephfs plugin daemonset %q", cephfsPlugin.Name)
		}
		k8sutil.AddRookVersionLabelToDaemonSet(cephfsPlugin)
	}

	if cephfsProvisionerDeployment != nil {
		// get CephFS provisioner tolerations and node affinity, defaults to common tolerations and node affinity if not specified
		cephFSProvisionerTolerations := getToleration(r.opConfig.Parameters, cephFSProvisionerTolerationsEnv, provisionerTolerations)
		cephFSProvisionerNodeAffinity := getNodeAffinity(r.opConfig.Parameters, cephFSProvisionerNodeAffinityEnv, provisionerNodeAffinity)
		// apply CephFS provisioner tolerations and node affinity
		applyToPodSpec(&cephfsProvisionerDeployment.Spec.Template.Spec, cephFSProvisionerNodeAffinity, cephFSProvisionerTolerations)
		// get resource details for cephfs provisioner
		// apply resource request and limit to cephfs provisioner containers
		applyResourcesToContainers(r.opConfig.Parameters, cephFSProvisionerResource, &cephfsProvisionerDeployment.Spec.Template.Spec)
		err = ownerInfo.SetControllerReference(cephfsProvisionerDeployment)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to cephfs provisioner deployment %q", cephfsProvisionerDeployment.Name)
		}
		antiAffinity := GetPodAntiAffinity("app", csiCephFSProvisioner)
		cephfsProvisionerDeployment.Spec.Template.Spec.Affinity.PodAntiAffinity = &antiAffinity
		cephfsProvisionerDeployment.Spec.Strategy = apps.DeploymentStrategy{
			Type: apps.RecreateDeploymentStrategyType,
		}

		err = r.applyCephClusterNetworkConfig(r.opManagerContext, &cephfsProvisionerDeployment.Spec.Template.ObjectMeta)
		if err != nil {
			return errors.Wrapf(err, "failed to apply network config to cephfs plugin provisioner deployment %q", cephfsProvisionerDeployment.Name)
		}

		_, err = k8sutil.CreateOrUpdateDeployment(r.opManagerContext, r.context.Clientset, cephfsProvisionerDeployment)
		if err != nil {
			return errors.Wrapf(err, "failed to start cephfs provisioner deployment %q", cephfsProvisionerDeployment.Name)
		}
		k8sutil.AddRookVersionLabelToDeployment(cephfsProvisionerDeployment)
		logger.Info("successfully started CSI CephFS driver")
	}
	if cephfsService != nil {
		err = ownerInfo.SetControllerReference(cephfsService)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to cephfs service %q", cephfsService)
		}
		_, err = k8sutil.CreateOrUpdateService(r.opManagerContext, r.context.Clientset, r.opConfig.OperatorNamespace, cephfsService)
		if err != nil {
			return errors.Wrapf(err, "failed to create cephfs service %q", cephfsService.Name)
		}
	}

	if nfsPlugin != nil {
		// get NFS plugin tolerations and node affinity, defaults to common tolerations and node affinity if not specified
		nfsPluginTolerations := getToleration(r.opConfig.Parameters, nfsPluginTolerationsEnv, pluginTolerations)
		nfsPluginNodeAffinity := getNodeAffinity(r.opConfig.Parameters, nfsPluginNodeAffinityEnv, pluginNodeAffinity)
		// apply NFS plugin tolerations and node affinity
		applyToPodSpec(&nfsPlugin.Spec.Template.Spec, nfsPluginNodeAffinity, nfsPluginTolerations)
		// apply resource request and limit to nfs plugin containers
		applyResourcesToContainers(r.opConfig.Parameters, nfsPluginResource, &nfsPlugin.Spec.Template.Spec)
		// apply custom mounts to volumes
		applyVolumeToPodSpec(r.opConfig.Parameters, nfsPluginVolume, &nfsPlugin.Spec.Template.Spec)
		// apply custom mounts to volume mounts
		applyVolumeMountToContainer(r.opConfig.Parameters, nfsPluginVolumeMount, "csi-nfsplugin", &nfsPlugin.Spec.Template.Spec)
		err = ownerInfo.SetControllerReference(nfsPlugin)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to nfs plugin daemonset %q", nfsPlugin.Name)
		}
		err = r.applyCephClusterNetworkConfig(r.opManagerContext, &nfsPlugin.Spec.Template.ObjectMeta)
		if err != nil {
			return errors.Wrapf(err, "failed to apply network config to nfs plugin daemonset %q", nfsPlugin.Name)
		}
		if holderEnabled {
			nfsPlugin.Spec.Template.Spec.HostNetwork = false
			// HostPID is used to communicate with the network namespace
			nfsPlugin.Spec.Template.Spec.HostPID = true
		}

		err = k8sutil.CreateDaemonSet(r.opManagerContext, r.opConfig.OperatorNamespace, r.context.Clientset, nfsPlugin)
		if err != nil {
			return errors.Wrapf(err, "failed to start nfs plugin daemonset %q", nfsPlugin.Name)
		}
		k8sutil.AddRookVersionLabelToDaemonSet(nfsPlugin)
	}

	if nfsProvisionerDeployment != nil {
		// get NFS provisioner tolerations and node affinity, defaults to common tolerations and node affinity if not specified
		nfsProvisionerTolerations := getToleration(r.opConfig.Parameters, nfsProvisionerTolerationsEnv, provisionerTolerations)
		nfsProvisionerNodeAffinity := getNodeAffinity(r.opConfig.Parameters, nfsProvisionerNodeAffinityEnv, provisionerNodeAffinity)
		// apply NFS provisioner tolerations and node affinity
		applyToPodSpec(&nfsProvisionerDeployment.Spec.Template.Spec, nfsProvisionerNodeAffinity, nfsProvisionerTolerations)
		// get resource details for nfs provisioner
		// apply resource request and limit to nfs provisioner containers
		applyResourcesToContainers(r.opConfig.Parameters, nfsProvisionerResource, &nfsProvisionerDeployment.Spec.Template.Spec)
		err = ownerInfo.SetControllerReference(nfsProvisionerDeployment)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to nfs provisioner deployment %q", nfsProvisionerDeployment.Name)
		}
		antiAffinity := GetPodAntiAffinity("app", csiNFSProvisioner)
		nfsProvisionerDeployment.Spec.Template.Spec.Affinity.PodAntiAffinity = &antiAffinity
		nfsProvisionerDeployment.Spec.Strategy = apps.DeploymentStrategy{
			Type: apps.RecreateDeploymentStrategyType,
		}

		err = r.applyCephClusterNetworkConfig(r.opManagerContext, &nfsProvisionerDeployment.Spec.Template.ObjectMeta)
		if err != nil {
			return errors.Wrapf(err, "failed to apply network config to nfs provisioner deployment %q", nfsProvisionerDeployment.Name)
		}
		_, err = k8sutil.CreateOrUpdateDeployment(r.opManagerContext, r.context.Clientset, nfsProvisionerDeployment)
		if err != nil {
			return errors.Wrapf(err, "failed to start nfs provisioner deployment %q", nfsProvisionerDeployment.Name)
		}
		k8sutil.AddRookVersionLabelToDeployment(nfsProvisionerDeployment)
		logger.Info("successfully started CSI NFS driver")
	}

	if EnableRBD {
		err = csiDriverobj.createCSIDriverInfo(
			r.opManagerContext, r.context.Clientset,
			RBDDriverName, k8sutil.GetValue(r.opConfig.Parameters, "CSI_RBD_FSGROUPPOLICY", string(k8scsi.FileFSGroupPolicy)),
			tp.Param.RBDAttachRequired, CSIParam.EnableCSIDriverSeLinuxMount)
		if err != nil {
			return errors.Wrapf(err, "failed to create CSI driver object for %q", RBDDriverName)
		}
	}
	if EnableCephFS {
		err = csiDriverobj.createCSIDriverInfo(
			r.opManagerContext, r.context.Clientset,
			CephFSDriverName, k8sutil.GetValue(r.opConfig.Parameters, "CSI_CEPHFS_FSGROUPPOLICY", string(k8scsi.FileFSGroupPolicy)),
			tp.Param.CephFSAttachRequired, CSIParam.EnableCSIDriverSeLinuxMount)
		if err != nil {
			return errors.Wrapf(err, "failed to create CSI driver object for %q", CephFSDriverName)
		}
	}
	if EnableNFS {
		err = csiDriverobj.createCSIDriverInfo(r.opManagerContext, r.context.Clientset,
			NFSDriverName, k8sutil.GetValue(r.opConfig.Parameters, "CSI_NFS_FSGROUPPOLICY", string(k8scsi.FileFSGroupPolicy)),
			tp.Param.NFSAttachRequired, CSIParam.EnableCSIDriverSeLinuxMount)
		if err != nil {
			return errors.Wrapf(err, "failed to create CSI driver object for %q", NFSDriverName)
		}
	}

	return nil
}

func (r *ReconcileCSI) stopDrivers() error {
	RBDDriverName = fmt.Sprintf("%s.rbd.csi.ceph.com", r.opConfig.OperatorNamespace)
	CephFSDriverName = fmt.Sprintf("%s.cephfs.csi.ceph.com", r.opConfig.OperatorNamespace)
	NFSDriverName = fmt.Sprintf("%s.nfs.csi.ceph.com", r.opConfig.OperatorNamespace)

	if !EnableRBD || EnableCSIOperator() {
		logger.Debugf("either EnableRBD if `false` or EnableCSIOperator is `true`, `EnableRBD is %t` and `EnableCSIOperator is %t", EnableRBD, EnableCSIOperator())
		err := r.deleteCSIDriverResources(CsiRBDPlugin, csiRBDProvisioner, "csi-rbdplugin-metrics", RBDDriverName)
		if err != nil {
			return errors.Wrap(err, "failed to remove CSI Ceph RBD driver")
		}
		logger.Info("successfully removed CSI Ceph RBD driver")
	}

	if !EnableCephFS || EnableCSIOperator() {
		logger.Debugf("either EnableCephFS if `false` or EnableCSIOperator is `true`, `EnableCephFS is %t` and `EnableCSIOperator is %t", EnableRBD, EnableCSIOperator())
		err := r.deleteCSIDriverResources(CsiCephFSPlugin, csiCephFSProvisioner, "csi-cephfsplugin-metrics", CephFSDriverName)
		if err != nil {
			return errors.Wrap(err, "failed to remove CSI CephFS driver")
		}
		logger.Info("successfully removed CSI CephFS driver")
	}

	if !EnableNFS || EnableCSIOperator() {
		logger.Debugf("either EnableNFS if `false` or EnableCSIOperator is `true`, `EnableNFS is %t` and `EnableCSIOperator is %t", EnableRBD, EnableCSIOperator())
		err := r.deleteCSIDriverResources(CsiNFSPlugin, csiNFSProvisioner, "csi-nfsplugin-metrics", NFSDriverName)
		if err != nil {
			return errors.Wrap(err, "failed to remove CSI NFS driver")
		}
		logger.Info("successfully removed CSI NFS driver")
	}

	return nil
}

func (r *ReconcileCSI) deleteCSIDriverResources(daemonset, deployment, service, driverName string) error {
	csiDriverobj := v1CsiDriver{}
	err := k8sutil.DeleteDaemonset(r.opManagerContext, r.context.Clientset, r.opConfig.OperatorNamespace, daemonset)
	if err != nil {
		return errors.Wrapf(err, "failed to delete the %q", daemonset)
	}

	err = k8sutil.DeleteDeployment(r.opManagerContext, r.context.Clientset, r.opConfig.OperatorNamespace, deployment)
	if err != nil {
		return errors.Wrapf(err, "failed to delete the %q", deployment)
	}

	err = k8sutil.DeleteService(r.opManagerContext, r.context.Clientset, r.opConfig.OperatorNamespace, service)
	if err != nil {
		return errors.Wrapf(err, "failed to delete the %q", service)
	}

	if !EnableCSIOperator() {
		err = csiDriverobj.deleteCSIDriverInfo(r.opManagerContext, r.context.Clientset, driverName)
		if err != nil {
			return errors.Wrapf(err, "failed to delete %q Driver Info", driverName)
		}
	}

	return nil
}

func (r *ReconcileCSI) applyCephClusterNetworkConfig(ctx context.Context, objectMeta *metav1.ObjectMeta) error {
	cephClusters, err := r.context.RookClientset.CephV1().CephClusters(objectMeta.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to find CephClusters")
	}
	for i, cephCluster := range cephClusters.Items {
		if cephCluster.Spec.Network.IsMultus() {
			err = k8sutil.ApplyMultus(cephCluster.GetNamespace(), &cephClusters.Items[i].Spec.Network, objectMeta)
			if err != nil {
				return errors.Wrapf(err, "failed to apply multus configuration to CephCluster %q", cephCluster.Name)
			}
		}
	}

	return nil
}

func (r *ReconcileCSI) configureHolders(enabledDrivers []driverDetails, tp templateParam, pluginTolerations []corev1.Toleration, pluginNodeAffinity *corev1.NodeAffinity) error {
	for _, cluster := range r.clustersWithHolder {
		for _, driver := range enabledDrivers {
			err := r.configureHolder(driver, cluster, tp, pluginTolerations, pluginNodeAffinity)
			if err != nil {
				return errors.Wrapf(err, "failed to configure holder %q for %q/%q", driver.name, cluster.cluster.Name, cluster.cluster.Namespace)
			}
		}
	}

	return nil
}

func (r *ReconcileCSI) configureHolder(driver driverDetails, c ClusterDetail, tp templateParam, pluginTolerations []corev1.Toleration, pluginNodeAffinity *corev1.NodeAffinity) error {
	cephPluginHolder, err := templateToDaemonSet("cephpluginholder", driver.holderTemplate, tp)
	if err != nil {
		return errors.Wrapf(err, "failed to load ceph %q plugin holder template", driver.fullName)
	}

	// As the plugin holder daemonset is created in the operator namespace, we
	// need to set the owner reference to the cluster namespace only if the
	// operator and cluster are created in same namespace so that the
	// plugin holder daemonset is deleted when the cluster is deleted.
	if r.opConfig.OperatorNamespace == c.cluster.Namespace {
		err = c.clusterInfo.OwnerInfo.SetControllerReference(cephPluginHolder)
		if err != nil {
			return errors.Wrapf(err, "failed to set owner reference to plugin holder %q", driver.fullName)
		}
	}
	holderPluginTolerations := getToleration(r.opConfig.Parameters, driver.toleration, pluginTolerations)
	holderPluginNodeAffinity := getNodeAffinity(r.opConfig.Parameters, driver.nodeAffinity, pluginNodeAffinity)
	// apply driver's plugin tolerations and node affinity
	applyToPodSpec(&cephPluginHolder.Spec.Template.Spec, holderPluginNodeAffinity, holderPluginTolerations)

	// apply resource request and limit from corresponding plugin container
	applyResourcesToContainers(r.opConfig.Parameters, driver.resource, &cephPluginHolder.Spec.Template.Spec)

	// Append the CEPH_CLUSTER_NAMESPACE env var so that the main container can use it to create the network
	// namespace symlink to the Kubelet plugin directory
	cephPluginHolder.Spec.Template.Spec.Containers[0].Env = append(
		cephPluginHolder.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{
			Name:  "CEPH_CLUSTER_NAMESPACE",
			Value: c.cluster.Namespace,
		},
	)

	// Append the driver name so that the symlink file goes into the right location on the
	// kubelet plugin directory
	cephPluginHolder.Spec.Template.Spec.Containers[0].Env = append(
		cephPluginHolder.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{
			Name:  "ROOK_CEPH_CSI_DRIVER_NAME",
			Value: driver.fullName,
		},
	)

	// Make the DS name unique per Ceph cluster
	cephPluginHolder.Name = fmt.Sprintf("%s-%s", cephPluginHolder.Name, c.cluster.Name)
	cephPluginHolder.Spec.Template.Name = cephPluginHolder.Name
	cephPluginHolder.Spec.Template.Spec.Containers[0].Name = fmt.Sprintf("%s-%s", cephPluginHolder.Spec.Template.Spec.Containers[0].Name, c.cluster.Name)

	// Add default labels
	k8sutil.AddRookVersionLabelToDaemonSet(cephPluginHolder)

	// If multus is enabled, add the multus plugin label
	if c.cluster.Spec.Network.IsMultus() {
		// Apply Multus annotations to daemonset spec
		err = k8sutil.ApplyMultus(c.cluster.GetNamespace(), &c.cluster.Spec.Network, &cephPluginHolder.Spec.Template.ObjectMeta)
		if err != nil {
			return errors.Wrapf(err, "failed to apply multus configuration for holder %q in cluster %q", cephPluginHolder.Name, c.cluster.Namespace)
		}
	}
	// Finally create the DaemonSet
	_, err = r.context.Clientset.AppsV1().DaemonSets(r.opConfig.OperatorNamespace).Create(r.opManagerContext, cephPluginHolder, metav1.CreateOptions{})
	if err != nil {
		if kerrors.IsAlreadyExists(err) {
			_, err = r.context.Clientset.AppsV1().DaemonSets(r.opConfig.OperatorNamespace).Update(r.opManagerContext, cephPluginHolder, metav1.UpdateOptions{})
			if err != nil {
				return errors.Wrapf(err, "failed to update ceph plugin holder daemonset %q", cephPluginHolder.Name)
			}
			logger.Debugf("holder %q already exists for cluster %q, updating it, restart holder pods to take effect of update", cephPluginHolder.Name, c.cluster.Namespace)
		} else {
			return errors.Wrapf(err, "failed to start ceph plugin holder daemonset %q", cephPluginHolder.Name)
		}
	}

	clusterConfigEntry := &CSIClusterConfigEntry{
		ClusterInfo: cephcsi.ClusterInfo{
			Monitors: MonEndpoints(c.clusterInfo.Monitors, c.cluster.Spec.RequireMsgr2()),
			RBD:      cephcsi.RBD{},
			CephFS: cephcsi.CephFS{
				FuseMountOptions:   c.clusterInfo.CSIDriverSpec.CephFS.FuseMountOptions,
				KernelMountOptions: c.clusterInfo.CSIDriverSpec.CephFS.KernelMountOptions,
			},
			NFS: cephcsi.NFS{},
			ReadAffinity: cephcsi.ReadAffinity{
				Enabled:             c.clusterInfo.CSIDriverSpec.ReadAffinity.Enabled,
				CrushLocationLabels: c.clusterInfo.CSIDriverSpec.ReadAffinity.CrushLocationLabels,
			},
		},
	}

	netNamespaceFilePath := generateNetNamespaceFilePath(CSIParam.KubeletDirPath, driver.fullName, c.cluster.Namespace)
	if driver.name == RBDDriverShortName {
		clusterConfigEntry.RBD.NetNamespaceFilePath = netNamespaceFilePath
	}
	if driver.name == CephFSDriverShortName {
		clusterConfigEntry.CephFS.NetNamespaceFilePath = netNamespaceFilePath
	}
	if driver.name == NFSDriverShortName {
		clusterConfigEntry.NFS.NetNamespaceFilePath = netNamespaceFilePath
	}

	// Save the path of the network namespace file for ceph-csi to use
	clusterId := c.cluster.Namespace // cluster ID is same as cluster namespace for CephClusters
	err = SaveClusterConfig(r.context.Clientset, clusterId, c.cluster.Namespace, c.clusterInfo, clusterConfigEntry)
	if err != nil {
		return errors.Wrapf(err, "failed to save cluster config for csi holder %q", driver.fullName)
	}
	return nil
}

func GenerateNetNamespaceFilePath(ctx context.Context, client client.Client, clusterNamespace, opNamespace, driverName string) (string, error) {
	var driverSuffix string
	opNamespaceName := types.NamespacedName{Name: opcontroller.OperatorSettingConfigMapName, Namespace: opNamespace}
	opConfig := &corev1.ConfigMap{}
	err := client.Get(ctx, opNamespaceName, opConfig)
	if err != nil && !kerrors.IsNotFound(err) {
		return "", errors.Wrap(err, "failed to get operator's configmap")
	}

	// net namespace file path is empty string if holder pods are disabled
	csiDisableHolders, err := strconv.ParseBool(k8sutil.GetValue(opConfig.Data, "CSI_DISABLE_HOLDER_PODS", "false"))
	if err != nil {
		return "", errors.Wrap(err, "failed to parse value for 'CSI_DISABLE_HOLDER_PODS'")
	}
	if csiDisableHolders {
		return "", nil
	}

	switch driverName {
	case RBDDriverShortName:
		driverSuffix = rbdDriverSuffix
	case CephFSDriverShortName:
		driverSuffix = cephFSDriverSuffix
	default:
		return "", errors.Errorf("unsupported driver name %q", driverName)
	}

	kubeletDirPath := k8sutil.GetValue(opConfig.Data, "ROOK_CSI_KUBELET_DIR_PATH", DefaultKubeletDirPath)
	driverFullName := fmt.Sprintf("%s.%s", opNamespace, driverSuffix)

	return generateNetNamespaceFilePath(kubeletDirPath, driverFullName, clusterNamespace), nil
}

func generateNetNamespaceFilePath(kubeletDirPath, driverFullName, clusterNamespace string) string {
	return fmt.Sprintf("%s/plugins/%s/%s.net.ns", kubeletDirPath, driverFullName, clusterNamespace)
}

func validateCSIDriverNamePrefix(ctx context.Context, clientset kubernetes.Interface, namespace, driverNamePrefix string) error {
	if EnableRBD {
		rbdDriverNamePrefix, err := getCSIDriverNamePrefixFromDeployment(ctx, clientset, namespace, csiRBDProvisioner, "csi-rbdplugin")
		if err != nil {
			return err
		}
		if rbdDriverNamePrefix != "" {
			if rbdDriverNamePrefix != driverNamePrefix {
				return errors.Errorf("rbd driver already exists with prefix %q, cannot use prefix %q", rbdDriverNamePrefix, driverNamePrefix)
			}
			return nil
		}
	}

	if EnableCephFS {
		cephFSDriverNamePrefix, err := getCSIDriverNamePrefixFromDeployment(ctx, clientset, namespace, csiCephFSProvisioner, "csi-cephfsplugin")
		if err != nil {
			return err
		}
		if cephFSDriverNamePrefix != "" {
			if cephFSDriverNamePrefix != driverNamePrefix {
				return errors.Errorf("cephFS driver already exists with prefix %q, cannot use prefix %q", cephFSDriverNamePrefix, driverNamePrefix)
			}
			return nil

		}
	}

	if EnableNFS {
		nfsDriverNamePrefix, err := getCSIDriverNamePrefixFromDeployment(ctx, clientset, namespace, csiNFSProvisioner, "csi-nfsplugin")
		if err != nil {
			return err
		}
		if nfsDriverNamePrefix != "" {
			if nfsDriverNamePrefix != driverNamePrefix {
				return errors.Errorf("nfs driver already exists with prefix %q, cannot use prefix %q", nfsDriverNamePrefix, driverNamePrefix)
			}
			return nil
		}
	}

	return nil
}

func getCSIDriverNamePrefixFromDeployment(ctx context.Context, clientset kubernetes.Interface, namespace, deploymentName, containerName string) (string, error) {
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", errors.Wrapf(err, "failed to get deployment %q", deploymentName)
	}

	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == containerName {
			for _, arg := range container.Args {
				if prefix, ok := getPrefixFromArg(arg); ok {
					return prefix, nil
				}
			}
		}
	}

	return "", errors.Errorf("failed to get CSI driver name from deployment %q", deploymentName)
}

func getPrefixFromArg(arg string) (string, bool) {
	if strings.Contains(arg, "--drivername=") {
		driverName := strings.Split(arg, "=")[1]

		for _, suffix := range []string{rbdDriverSuffix, cephFSDriverSuffix, nfsDriverSuffix} {
			// Add a dot as we are adding it to the Prefix
			if prefix, ok := strings.CutSuffix(driverName, "."+suffix); ok {
				return prefix, true
			}
		}
	}
	return "", false
}

func EnableCSIOperator() bool {
	return enableCSIOperator && !IsHolderEnabled()
}
