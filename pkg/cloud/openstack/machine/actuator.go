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

package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/services/compute"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/services/networking"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"

	openstackconfigv1 "shiftstack/machine-api-provider-openstack/pkg/apis/openstackproviderconfig/v1alpha1"
	"shiftstack/machine-api-provider-openstack/pkg/bootstrap"
	"shiftstack/machine-api-provider-openstack/pkg/cloud/openstack"
	"shiftstack/machine-api-provider-openstack/pkg/cloud/openstack/clients"
	"shiftstack/machine-api-provider-openstack/pkg/cloud/openstack/options"

	"github.com/gophercloud/gophercloud"
	gophercloudopenstack "github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/gophercloud/utils/openstack/clientconfig"
	machinev1 "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	maoMachine "github.com/openshift/machine-api-operator/pkg/controller/machine"
	"github.com/openshift/machine-api-operator/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	tokenapi "k8s.io/cluster-bootstrap/token/api"
	tokenutil "k8s.io/cluster-bootstrap/token/util"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clconfig "github.com/coreos/container-linux-config-transpiler/config"
)

const (
	CloudConfigPath = "/etc/cloud/cloud_config.yaml"

	UserDataKey          = "userData"
	DisableTemplatingKey = "disableTemplating"
	PostprocessorKey     = "postprocessor"

	TimeoutInstanceCreate       = 5
	TimeoutInstanceDelete       = 5
	RetryIntervalInstanceStatus = 10 * time.Second

	// MachineInstanceStateAnnotationName as annotation name for a machine instance state
	MachineInstanceStateAnnotationName = "machine.openshift.io/instance-state"

	// ErrorState is assigned to the machine if its instance has been destroyed
	ErrorState = "ERROR"
)

// Event Action Constants
const (
	createEventAction = "Create"
	updateEventAction = "Update"
	deleteEventAction = "Delete"
	noEventAction     = ""
)

type OpenstackClient struct {
	params openstack.ActuatorParams
	scheme *runtime.Scheme
	client client.Client
	*openstack.DeploymentClient
	eventRecorder record.EventRecorder
}

func NewActuator(params openstack.ActuatorParams) (*OpenstackClient, error) {
	return &OpenstackClient{
		params:           params,
		client:           params.Client,
		scheme:           params.Scheme,
		DeploymentClient: openstack.NewDeploymentClient(),
		eventRecorder:    params.EventRecorder,
	}, nil
}

func getTimeout(name string, timeout int) time.Duration {
	if v := os.Getenv(name); v != "" {
		timeout, err := strconv.Atoi(v)
		if err == nil {
			return time.Duration(timeout)
		}
	}
	return time.Duration(timeout)
}

func (oc *OpenstackClient) getProviderClient(machine *machinev1.Machine) (*gophercloud.ProviderClient, *clientconfig.Cloud, error) {
	cloud, err := clients.GetCloud(oc.params.KubeClient, machine)
	if err != nil {
		return nil, nil, err
	}
	provider, err := clients.GetProviderClient(cloud, clients.GetCACertificate(oc.params.KubeClient))
	if err != nil {
		return nil, nil, err
	}

	return provider, &cloud, nil
}

func (oc *OpenstackClient) getUserData(machine *machinev1.Machine, providerSpec *openstackconfigv1.OpenstackProviderSpec, kubeClient kubernetes.Interface) (string, error) {
	// get machine startup script
	var ok bool
	var disableTemplating bool
	var postprocessor string
	var postprocess bool

	userData := []byte{}
	if providerSpec.UserDataSecret != nil {
		namespace := providerSpec.UserDataSecret.Namespace
		if namespace == "" {
			namespace = machine.Namespace
		}

		if providerSpec.UserDataSecret.Name == "" {
			return "", fmt.Errorf("UserDataSecret name must be provided")
		}

		userDataSecret, err := kubeClient.CoreV1().Secrets(namespace).Get(context.TODO(), providerSpec.UserDataSecret.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}

		userData, ok = userDataSecret.Data[UserDataKey]
		if !ok {
			return "", fmt.Errorf("Machine's userdata secret %v in namespace %v did not contain key %v", providerSpec.UserDataSecret.Name, namespace, UserDataKey)
		}

		_, disableTemplating = userDataSecret.Data[DisableTemplatingKey]

		var p []byte
		p, postprocess = userDataSecret.Data[PostprocessorKey]

		postprocessor = string(p)
	}

	var userDataRendered string
	var err error
	if len(userData) > 0 && !disableTemplating {
		// FIXME(mandre) Find the right way to check if machine is part of the control plane
		if machine.ObjectMeta.Name != "" {
			userDataRendered, err = masterStartupScript(machine, string(userData))
			if err != nil {
				return "", oc.handleMachineError(machine, maoMachine.CreateMachine(
					"error creating Openstack instance: %v", err), createEventAction)
			}
		} else {
			klog.Info("Creating bootstrap token")
			token, err := oc.createBootstrapToken()
			if err != nil {
				return "", oc.handleMachineError(machine, maoMachine.CreateMachine(
					"error creating Openstack instance: %v", err), createEventAction)
			}
			userDataRendered, err = nodeStartupScript(machine, token, string(userData))
			if err != nil {
				return "", oc.handleMachineError(machine, maoMachine.CreateMachine(
					"error creating Openstack instance: %v", err), createEventAction)
			}
		}
	} else {
		userDataRendered = string(userData)
	}

	if postprocess {
		switch postprocessor {
		// Postprocess with the Container Linux ct transpiler.
		case "ct":
			clcfg, ast, report := clconfig.Parse([]byte(userDataRendered))
			if len(report.Entries) > 0 {
				return "", fmt.Errorf("Postprocessor error: %s", report.String())
			}

			ignCfg, report := clconfig.Convert(clcfg, "openstack-metadata", ast)
			if len(report.Entries) > 0 {
				return "", fmt.Errorf("Postprocessor error: %s", report.String())
			}

			ud, err := json.Marshal(&ignCfg)
			if err != nil {
				return "", fmt.Errorf("Postprocessor error: %s", err)
			}

			userDataRendered = string(ud)

		default:
			return "", fmt.Errorf("Postprocessor error: unknown postprocessor: '%s'", postprocessor)
		}
	}

	return userDataRendered, nil
}

func setMachineLabels(machine *machinev1.Machine, region, availability_zone, flavor string) {
	// Don't update labels which have already been set
	if machine.Labels[maoMachine.MachineRegionLabelName] != "" && machine.Labels[maoMachine.MachineAZLabelName] != "" && machine.Labels[maoMachine.MachineInstanceTypeLabelName] != "" {
		return
	}

	if machine.Labels == nil {
		machine.Labels = make(map[string]string)
	}

	// Set the region
	machine.Labels[maoMachine.MachineRegionLabelName] = region

	// Set the availability zone
	machine.Labels[maoMachine.MachineAZLabelName] = availability_zone

	// Set the flavor name
	machine.Labels[maoMachine.MachineInstanceTypeLabelName] = flavor
}

func (oc *OpenstackClient) Create(ctx context.Context, machine *machinev1.Machine) error {
	// First check that provided labels are correct
	// TODO(mfedosin): stop sending the infrastructure request when we start to receive the cluster value
	clusterInfra, err := oc.params.ConfigClient.Infrastructures().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Failed to retrieve cluster Infrastructure object: %v", err)
	}

	clusterInfraName := clusterInfra.Status.InfrastructureName
	clusterNameLabel := machine.Labels["machine.openshift.io/cluster-api-cluster"]

	if clusterNameLabel != clusterInfraName {
		klog.Errorf("machine.openshift.io/cluster-api-cluster label value is incorrect: %v, machine %v cannot join cluster %v", clusterNameLabel, machine.ObjectMeta.Name, clusterInfraName)
		verr := maoMachine.InvalidMachineConfiguration("machine.openshift.io/cluster-api-cluster label value is incorrect: %v, machine %v cannot join cluster %v", clusterNameLabel, machine.ObjectMeta.Name, clusterInfraName)

		return oc.handleMachineError(machine, verr, createEventAction)
	}

	kubeClient := oc.params.KubeClient
	provider, cloud, err := oc.getProviderClient(machine)
	if err != nil {
		return err
	}

	clientOpts := clientconfig.ClientOpts{
		AuthInfo:   cloud.AuthInfo,
		RegionName: cloud.RegionName,
	}

	computeService, err := compute.NewService(provider, &clientOpts, ctrl.Log)
	if err != nil {
		return err
	}

	networkService, err := networking.NewService(provider, &clientOpts, ctrl.Log)
	if err != nil {
		return err
	}

	providerSpec, err := openstackconfigv1.MachineSpecFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return oc.handleMachineError(machine, maoMachine.InvalidMachineConfiguration(
			"Cannot unmarshal providerSpec field: %v", err), createEventAction)
	}

	if err = oc.validateMachine(machine); err != nil {
		verr := maoMachine.InvalidMachineConfiguration("Machine validation failed: %v", err)
		return oc.handleMachineError(machine, verr, createEventAction)
	}

	instanceStatus, err := computeService.GetInstanceStatusByName(machine, machine.Name)
	if err != nil {
		return err
	}
	if instanceStatus != nil {
		klog.Infof("Skipped creating a VM that already exists.\n")
		return nil
	}

	// Here we check whether we want to create a new instance or recreate the destroyed
	// one. If this is the second case, we have to return an error, because if we just
	// create an instance with the old name, because the CSR for it will not be approved
	// automatically.
	// See https://bugzilla.redhat.com/show_bug.cgi?id=1746369
	if machine.ObjectMeta.Annotations[InstanceStatusAnnotationKey] != "" {
		klog.Errorf("The instance has been destroyed for the machine %v, cannot recreate it.\n", machine.ObjectMeta.Name)
		verr := maoMachine.InvalidMachineConfiguration("the instance has been destroyed for the machine %v, cannot recreate it.\n", machine.ObjectMeta.Name)

		return oc.handleMachineError(machine, verr, createEventAction)
	}

	userDataRendered, err := oc.getUserData(machine, providerSpec, kubeClient)
	if err != nil {
		return err
	}

	// Read the cluster name from the `machine`.
	clusterName := fmt.Sprintf("%s-%s", machine.Namespace, machine.Labels["machine.openshift.io/cluster-api-cluster"])

	// TODO(egarcia): if we ever use the cluster object, this will benifit from reading from it
	var clusterSpec openstackconfigv1.OpenstackClusterProviderSpec

	// Convert to v1alpha4
	osMachine, err := openstackconfigv1.NewOpenStackMachine(machine)
	if err != nil {
		return err
	}
	osCluster := openstackconfigv1.NewOpenStackCluster(clusterSpec, openstackconfigv1.OpenstackClusterProviderStatus{})
	if err != nil {
		return err
	}

	// XXX(mdbooth): v1Machine is also used to set security group based on IsControlPlaneMachine
	v1Machine := clusterv1.Machine{}
	v1Machine.Spec.FailureDomain = &providerSpec.AvailabilityZone
	instanceStatus, err = computeService.CreateInstance(&osCluster, &v1Machine, osMachine, clusterName, userDataRendered)
	if err != nil {
		return oc.handleMachineError(machine, maoMachine.CreateMachine(
			"error creating Openstack instance: %v", err), createEventAction)
	}

	if providerSpec.FloatingIP != "" {
		fp, err := networkService.GetOrCreateFloatingIP(&osCluster, clusterName, providerSpec.FloatingIP)
		if err != nil {
			return oc.handleMachineError(machine, maoMachine.CreateMachine(
				"Get floatingIP err: %v", err), createEventAction)
		}
		port, err := computeService.GetManagementPort(instanceStatus)
		if err != nil {
			return oc.handleMachineError(machine, maoMachine.CreateMachine(
				"Get management port err: %v", err), createEventAction)
		}

		err = networkService.AssociateFloatingIP(&osCluster, fp, port.ID)
		if err != nil {
			return oc.handleMachineError(machine, maoMachine.CreateMachine(
				"Associate floatingIP err: %v", err), createEventAction)
		}
	}

	oc.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Created", "Created machine %v", machine.Name)

	setMachineLabels(machine, cloud.RegionName, instanceStatus.AvailabilityZone(), providerSpec.Flavor)
	return oc.updateAnnotation(machine, instanceStatus.ID(), clusterInfraName)
}

func (oc *OpenstackClient) Delete(ctx context.Context, machine *machinev1.Machine) error {
	provider, cloud, err := oc.getProviderClient(machine)
	if err != nil {
		return err
	}
	computeService, err := compute.NewService(provider, &clientconfig.ClientOpts{
		AuthInfo:   cloud.AuthInfo,
		RegionName: cloud.RegionName,
	}, ctrl.Log)
	if err != nil {
		return err
	}

	instanceStatus, err := computeService.GetInstanceStatusByName(machine, machine.Name)
	if err != nil {
		return oc.handleMachineError(machine, maoMachine.DeleteMachine(
			"error getting OpenStack instance: %v", err), deleteEventAction)
	}

	if instanceStatus == nil {
		klog.Infof("Skipped deleting %s that is already deleted.\n", machine.Name)
		return nil
	}

	var clusterSpec openstackconfigv1.OpenstackClusterProviderSpec
	osCluster := openstackconfigv1.NewOpenStackCluster(clusterSpec, openstackconfigv1.OpenstackClusterProviderStatus{})
	if err != nil {
		return err
	}
	err = computeService.DeleteInstance(&osCluster, instanceStatus)
	if err != nil {
		return oc.handleMachineError(machine, maoMachine.DeleteMachine(
			"error deleting Openstack instance: %v", err), deleteEventAction)
	}

	oc.eventRecorder.Eventf(machine, corev1.EventTypeNormal, "Deleted", "Deleted machine %v", machine.Name)
	return nil
}

func (oc *OpenstackClient) Update(ctx context.Context, machine *machinev1.Machine) error {
	if err := oc.validateMachine(machine); err != nil {
		verr := &maoMachine.MachineError{
			Reason:  machinev1.UpdateMachineError,
			Message: err.Error(),
		}
		return oc.handleMachineError(machine, verr, updateEventAction)
	}

	clusterInfra, err := oc.params.ConfigClient.Infrastructures().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Failed to retrieve cluster Infrastructure object: %v", err)
	}

	status, err := oc.instanceStatus(machine)
	if err != nil {
		return err
	}

	currentMachine := (*machinev1.Machine)(status)
	if currentMachine == nil {
		instance, err := oc.instanceExists(machine)
		if err != nil {
			return err
		}
		if instance != nil && instance.Status == "ACTIVE" {
			klog.Infof("Populating current state for boostrap machine %v", machine.ObjectMeta.Name)

			kubeClient := oc.params.KubeClient
			machineService, err := clients.NewInstanceServiceFromMachine(kubeClient, machine)
			if err != nil {
				return err
			}

			err = machineService.SetMachineLabels(machine, instance.ID)
			if err != nil {
				return nil
			}

			return oc.updateAnnotation(machine, instance.ID, clusterInfra.Status.InfrastructureName)
		} else {
			return fmt.Errorf("Cannot retrieve current state to update machine %v", machine.ObjectMeta.Name)
		}
	}

	if !oc.requiresUpdate(currentMachine, machine) {
		return nil
	}

	if _, ok := currentMachine.Labels["node-role.kubernetes.io/master"]; ok {
		// In this conditional block, Machine is Control Plane
		// TODO: add master inplace
		klog.Errorf("master inplace update failed: not supported")
		return oc.handleMachineError(machine, maoMachine.UpdateMachine(
			"master inplace update failed: not supported"), updateEventAction)
	} else {
		// In this conditional block, Machine is Compute Node
		klog.Infof("re-creating machine %s for update.", currentMachine.ObjectMeta.Name)
		err = oc.Create(ctx, machine)
		if err != nil {
			klog.Errorf("create machine %s for update failed: %v", machine.ObjectMeta.Name, err)
			return fmt.Errorf("Cannot create machine %s: %v", machine.ObjectMeta.Name, err)
		}

		err = oc.Delete(ctx, currentMachine)
		if err != nil {
			klog.Errorf("delete machine %s for update failed: %v", currentMachine.ObjectMeta.Name, err)
			return fmt.Errorf("Cannot delete machine %s: %v", currentMachine.ObjectMeta.Name, err)
		}
		instanceDeleteTimeout := getTimeout("CLUSTER_API_OPENSTACK_INSTANCE_DELETE_TIMEOUT", TimeoutInstanceDelete)
		instanceDeleteTimeout = instanceDeleteTimeout * time.Minute
		err = util.PollImmediate(RetryIntervalInstanceStatus, instanceDeleteTimeout, func() (bool, error) {
			instance, err := oc.instanceExists(machine)
			if err != nil {
				return false, nil
			}
			return instance == nil, nil
		})
		if err != nil {
			return oc.handleMachineError(machine, maoMachine.DeleteMachine(
				"error deleting Openstack instance: %v", err), updateEventAction)
		}
		klog.Infof("Successfully updated machine %s", currentMachine.ObjectMeta.Name)
	}

	oc.eventRecorder.Eventf(currentMachine, corev1.EventTypeNormal, "Updated", "Updated machine %v", currentMachine.ObjectMeta.Name)
	return nil
}

func (oc *OpenstackClient) Exists(ctx context.Context, machine *machinev1.Machine) (bool, error) {
	instance, err := oc.instanceExists(machine)
	if err != nil {
		return false, fmt.Errorf("Error checking if instance exists (machine/actuator.go 346): %v", err)
	}
	return instance != nil, err
}

func getIPsFromInstance(instance *clients.Instance) (map[string]string, error) {
	if instance.AccessIPv4 != "" && net.ParseIP(instance.AccessIPv4) != nil {
		return map[string]string{
			"": instance.AccessIPv4,
		}, nil
	}
	type networkInterface struct {
		Address string  `json:"addr"`
		Version float64 `json:"version"`
		Type    string  `json:"OS-EXT-IPS:type"`
	}
	addrMap := map[string]string{}

	for networkName, b := range instance.Addresses {
		list, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("extract IP from instance err: %v", err)
		}
		var networks []interface{}
		json.Unmarshal(list, &networks)
		for _, network := range networks {
			var netInterface networkInterface
			b, _ := json.Marshal(network)
			json.Unmarshal(b, &netInterface)
			if netInterface.Version == 4.0 {
				addrMap[networkName] = netInterface.Address
			}
		}
	}
	if len(addrMap) == 0 {
		return nil, fmt.Errorf("extract IP from instance err")
	}

	return addrMap, nil
}

func getNetworkBySubnet(client *gophercloud.ServiceClient, subnetID string) (*networks.Network, error) {
	subnet, err := subnets.Get(client, subnetID).Extract()
	if err != nil {
		return nil, fmt.Errorf("Could not get subnet %s, %v", subnetID, err)
	}

	network, err := networks.Get(client, subnet.NetworkID).Extract()
	if err != nil {
		return nil, fmt.Errorf("Could not get network %s, %v", subnet.NetworkID, err)
	}
	return network, nil
}

func getNetworkByPrimaryNetworkTag(client *gophercloud.ServiceClient, primaryNetworkTag string) (*networks.Network, error) {
	opts := networks.ListOpts{
		Tags: primaryNetworkTag,
	}

	allPages, err := networks.List(client, opts).AllPages()
	if err != nil {
		return nil, err
	}

	allNetworks, err := networks.ExtractNetworks(allPages)
	if err != nil {
		return nil, err
	}

	switch len(allNetworks) {
	case 0:
		return nil, fmt.Errorf("There are no networks with primary network tag: %v", primaryNetworkTag)
	case 1:
		return &allNetworks[0], nil
	}
	return nil, fmt.Errorf("Too many networks with the same primary network tag: %v", primaryNetworkTag)
}

func (oc *OpenstackClient) getPrimaryMachineIP(mapAddr map[string]string, machine *machinev1.Machine, clusterInfraName string) (string, error) {
	// If there is only one network in the list, we consider it as the primary one
	if len(mapAddr) == 1 {
		for _, addr := range mapAddr {
			return addr, nil
		}
	}

	config, err := openstackconfigv1.MachineSpecFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return "", fmt.Errorf("Invalid provider spec for machine %s", machine.Name)
	}

	// PrimarySubnet should always be set in the machine api in 4.6
	primarySubnet := config.PrimarySubnet

	provider, cloud, err := oc.getProviderClient(machine)
	if err != nil {
		return "", err
	}
	netClient, err := gophercloudopenstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: cloud.RegionName,
	})
	if err != nil {
		return "", err
	}

	var primaryNetwork *networks.Network
	if primarySubnet != "" {
		primaryNetwork, err = getNetworkBySubnet(netClient, primarySubnet)
		if err != nil {
			return "", err
		}
	} else {
		// Support legacy versions
		primaryNetworkTag := clusterInfraName + "-primaryClusterNetwork"
		primaryNetwork, err = getNetworkByPrimaryNetworkTag(netClient, primaryNetworkTag)
		if err != nil {
			return "", err
		}
	}

	for networkName, addr := range mapAddr {
		if networkName == primaryNetwork.Name {
			return addr, nil
		}
	}

	return "", fmt.Errorf("No primary network was found for the machine %v", machine.Name)
}

// If the OpenstackClient has a client for updating Machine objects, this will set
// the appropriate reason/message on the Machine.Status. If not, such as during
// cluster installation, it will operate as a no-op. It also returns the
// original error for convenience, so callers can do "return handleMachineError(...)".
func (oc *OpenstackClient) handleMachineError(machine *machinev1.Machine, err *maoMachine.MachineError, eventAction string) error {
	if eventAction != noEventAction {
		oc.eventRecorder.Eventf(machine, corev1.EventTypeWarning, "Failed"+eventAction, "%v", err.Reason)
	}
	if oc.client != nil {
		reason := err.Reason
		message := err.Message
		machine.Status.ErrorReason = &reason
		machine.Status.ErrorMessage = &message

		// Set state label to indicate that this machine is broken
		if machine.ObjectMeta.Annotations == nil {
			machine.ObjectMeta.Annotations = make(map[string]string)
		}
		machine.ObjectMeta.Annotations[MachineInstanceStateAnnotationName] = ErrorState

		if err := oc.client.Update(context.TODO(), machine); err != nil {
			return fmt.Errorf("unable to update machine status: %v", err)
		}
	}

	klog.Errorf("Machine error %s: %v", machine.Name, err.Message)
	return err
}

func (oc *OpenstackClient) updateAnnotation(machine *machinev1.Machine, instanceID string, clusterInfraName string) error {
	statusCopy := *machine.Status.DeepCopy()

	if machine.ObjectMeta.Annotations == nil {
		machine.ObjectMeta.Annotations = make(map[string]string)
	}
	machine.ObjectMeta.Annotations[openstack.OpenstackIdAnnotationKey] = instanceID

	// XXX(mdbooth): In both places we call updateAnnotation(), instance is already available. We can pass it as an arg.
	instance, _ := oc.instanceExists(machine)
	mapAddr, err := getIPsFromInstance(instance)
	if err != nil {
		return err
	}

	// XXX(mdbooth): getPrimaryMachineIP uses a network client which we should already have
	primaryIP, err := oc.getPrimaryMachineIP(mapAddr, machine, clusterInfraName)
	if err != nil {
		return err
	}
	klog.Infof("Found the primary address for the machine %v: %v", machine.Name, primaryIP)

	machine.ObjectMeta.Annotations[openstack.OpenstackIPAnnotationKey] = primaryIP
	machine.ObjectMeta.Annotations[MachineInstanceStateAnnotationName] = instance.Status

	if err := oc.client.Update(context.TODO(), machine); err != nil {
		return err
	}

	networkAddresses := []corev1.NodeAddress{}
	networkAddresses = append(networkAddresses, corev1.NodeAddress{
		Type:    corev1.NodeInternalIP,
		Address: primaryIP,
	})

	networkAddresses = append(networkAddresses, corev1.NodeAddress{
		Type:    corev1.NodeHostName,
		Address: machine.Name,
	})

	networkAddresses = append(networkAddresses, corev1.NodeAddress{
		Type:    corev1.NodeInternalDNS,
		Address: machine.Name,
	})

	machineCopy := machine.DeepCopy()
	machineCopy.Status.Addresses = networkAddresses

	if !equality.Semantic.DeepEqual(machine.Status.Addresses, machineCopy.Status.Addresses) {
		if err := oc.client.Status().Update(context.TODO(), machineCopy); err != nil {
			return err
		}
	}

	machine.Status = statusCopy
	return oc.updateInstanceStatus(machine)
}

func (oc *OpenstackClient) requiresUpdate(a *machinev1.Machine, b *machinev1.Machine) bool {
	if a == nil || b == nil {
		return true
	}
	// Do not want status changes. Do want changes that impact machine provisioning
	return !reflect.DeepEqual(a.Spec.ObjectMeta, b.Spec.ObjectMeta) ||
		!reflect.DeepEqual(a.Spec.ProviderSpec, b.Spec.ProviderSpec) ||
		a.ObjectMeta.Name != b.ObjectMeta.Name
}

func (oc *OpenstackClient) instanceExists(machine *machinev1.Machine) (instance *clients.Instance, err error) {
	machineSpec, err := openstackconfigv1.MachineSpecFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, fmt.Errorf("\nError getting the machine spec from the provider spec (machine/actuator.go 457): %v", err)
	}
	opts := &clients.InstanceListOpts{
		Name:   machine.Name,
		Image:  machineSpec.Image,
		Flavor: machineSpec.Flavor,
	}

	machineService, err := clients.NewInstanceServiceFromMachine(oc.params.KubeClient, machine)
	if err != nil {
		return nil, fmt.Errorf("\nError getting a new instance service from the machine (machine/actuator.go 467): %v", err)
	}

	instanceList, err := machineService.GetInstanceList(opts)
	if err != nil {
		return nil, fmt.Errorf("\nError listing the instances: %v", err)
	}
	if len(instanceList) == 0 {
		return nil, nil
	}
	return instanceList[0], nil
}

func (oc *OpenstackClient) createBootstrapToken() (string, error) {
	token, err := tokenutil.GenerateBootstrapToken()
	if err != nil {
		return "", err
	}

	expiration := time.Now().UTC().Add(options.TokenTTL)
	tokenSecret, err := bootstrap.GenerateTokenSecret(token, expiration)
	if err != nil {
		panic(fmt.Sprintf("unable to create token. there might be a bug somwhere: %v", err))
	}

	err = oc.client.Create(context.TODO(), tokenSecret)
	if err != nil {
		return "", err
	}

	return tokenutil.TokenFromIDAndSecret(
		string(tokenSecret.Data[tokenapi.BootstrapTokenIDKey]),
		string(tokenSecret.Data[tokenapi.BootstrapTokenSecretKey]),
	), nil
}

func (oc *OpenstackClient) validateMachine(machine *machinev1.Machine) error {
	machineSpec, err := openstackconfigv1.MachineSpecFromProviderSpec(machine.Spec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("\nError getting the machine spec from the provider spec: %v", err)
	}

	machineService, err := clients.NewInstanceServiceFromMachine(oc.params.KubeClient, machine)
	if err != nil {
		return fmt.Errorf("\nError getting a new instance service from the machine: %v", err)
	}

	// TODO(mfedosin): add more validations here

	// Validate that image exists when not booting from volume
	if machineSpec.RootVolume == nil {
		err = machineService.DoesImageExist(machineSpec.Image)
		if err != nil {
			return err
		}
	}

	// Validate that flavor exists
	err = machineService.DoesFlavorExist(machineSpec.Flavor)
	if err != nil {
		return err
	}

	// Validate that Availability Zone exists
	err = machineService.DoesAvailabilityZoneExist(machineSpec.AvailabilityZone)
	if err != nil {
		return err
	}

	return nil
}
