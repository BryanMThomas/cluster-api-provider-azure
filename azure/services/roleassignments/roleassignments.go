/*
Copyright 2019 The Kubernetes Authors.

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

package roleassignments

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/profiles/2019-03-01/authorization/mgmt/authorization"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-04-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/pkg/errors"

	"sigs.k8s.io/cluster-api-provider-azure/azure"
	"sigs.k8s.io/cluster-api-provider-azure/azure/services/async"
	"sigs.k8s.io/cluster-api-provider-azure/azure/services/scalesets"
	"sigs.k8s.io/cluster-api-provider-azure/azure/services/virtualmachines"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
)

const azureBuiltInContributorID = "b24988ac-6180-42a0-ab88-20f7382dd24c"

// RoleAssignmentScope defines the scope interface for a role assignment service.
type RoleAssignmentScope interface {
	azure.ClusterDescriber
	RoleAssignmentSpecs() []azure.RoleAssignmentSpec
}

// Service provides operations on Azure resources.
type Service struct {
	Scope RoleAssignmentScope
	client
	virtualMachinesGetter        async.Getter
	virtualMachineScaleSetClient scalesets.Client
}

// New creates a new service.
func New(scope RoleAssignmentScope) *Service {
	return &Service{
		Scope:                        scope,
		client:                       newClient(scope),
		virtualMachinesGetter:        virtualmachines.NewClient(scope),
		virtualMachineScaleSetClient: scalesets.NewClient(scope),
	}
}

// Reconcile creates a role assignment.
func (s *Service) Reconcile(ctx context.Context) error {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "roleassignments.Service.Reconcile")
	defer done()

	for _, roleSpec := range s.Scope.RoleAssignmentSpecs() {
		switch roleSpec.ResourceType {
		case azure.VirtualMachine:
			return s.reconcileVM(ctx, roleSpec)
		case azure.VirtualMachineScaleSet:
			return s.reconcileVMSS(ctx, roleSpec)
		default:
			return errors.Errorf("unexpected resource type %q. Expected one of [%s, %s]", roleSpec.ResourceType,
				azure.VirtualMachine, azure.VirtualMachineScaleSet)
		}
	}
	return nil
}

func (s *Service) reconcileVM(ctx context.Context, roleSpec azure.RoleAssignmentSpec) error {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "roleassignments.Service.reconcileVM")
	defer done()

	spec := &virtualmachines.VMSpec{
		Name:          roleSpec.MachineName,
		ResourceGroup: s.Scope.ResourceGroup(),
	}

	resultVMIface, err := s.virtualMachinesGetter.Get(ctx, spec)
	if err != nil {
		return errors.Wrap(err, "cannot get VM to assign role to system assigned identity")
	}
	resultVM, ok := resultVMIface.(compute.VirtualMachine)
	if !ok {
		return errors.Errorf("%T is not a compute.VirtualMachine", resultVMIface)
	}

	err = s.assignRole(ctx, roleSpec.Name, resultVM.Identity.PrincipalID)
	if err != nil {
		return errors.Wrap(err, "cannot assign role to VM system assigned identity")
	}

	log.V(2).Info("successfully created role assignment for generated Identity for VM", "virtual machine", roleSpec.MachineName)

	return nil
}

func (s *Service) reconcileVMSS(ctx context.Context, roleSpec azure.RoleAssignmentSpec) error {
	ctx, log, done := tele.StartSpanWithLogger(ctx, "roleassignments.Service.reconcileVMSS")
	defer done()

	resultVMSS, err := s.virtualMachineScaleSetClient.Get(ctx, s.Scope.ResourceGroup(), roleSpec.MachineName)
	if err != nil {
		return errors.Wrap(err, "cannot get VMSS to assign role to system assigned identity")
	}

	err = s.assignRole(ctx, roleSpec.Name, resultVMSS.Identity.PrincipalID)
	if err != nil {
		return errors.Wrap(err, "cannot assign role to VMSS system assigned identity")
	}

	log.V(2).Info("successfully created role assignment for generated Identity for VMSS", "virtual machine scale set", roleSpec.MachineName)

	return nil
}

func (s *Service) assignRole(ctx context.Context, roleAssignmentName string, principalID *string) error {
	ctx, _, done := tele.StartSpanWithLogger(ctx, "roleassignments.Service.assignRole")
	defer done()

	scope := fmt.Sprintf("/subscriptions/%s/", s.Scope.SubscriptionID())
	// Azure built-in roles https://docs.microsoft.com/en-us/azure/role-based-access-control/built-in-roles
	contributorRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s", s.Scope.SubscriptionID(), azureBuiltInContributorID)
	params := authorization.RoleAssignmentCreateParameters{
		Properties: &authorization.RoleAssignmentProperties{
			RoleDefinitionID: to.StringPtr(contributorRoleDefinitionID),
			PrincipalID:      principalID,
		},
	}
	_, err := s.client.Create(ctx, scope, roleAssignmentName, params)
	return err
}

// Delete is a no-op as the role assignments get deleted as part of VM deletion.
func (s *Service) Delete(ctx context.Context) error {
	_, _, done := tele.StartSpanWithLogger(ctx, "roleassignments.Service.Delete")
	defer done()

	return nil
}
