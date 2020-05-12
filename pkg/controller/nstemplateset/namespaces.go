package nstemplateset

import (
	"context"

	toolchainv1alpha1 "github.com/codeready-toolchain/api/pkg/apis/toolchain/v1alpha1"
	applycl "github.com/codeready-toolchain/toolchain-common/pkg/client"
	"github.com/codeready-toolchain/toolchain-common/pkg/template"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/redhat-cop/operator-utils/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type namespacesManager struct {
	*statusManager
}

// ensure ensures that all expected namespaces exists and they contain all the expected resources
// return `true, nil` when something changed, `false, nil` or `false, err` otherwise
func (r *namespacesManager) ensure(logger logr.Logger, nsTmplSet *toolchainv1alpha1.NSTemplateSet) (createdOrUpdated bool, err error) {
	username := nsTmplSet.GetName()
	userNamespaces, err := fetchNamespaces(r.client, username)
	if err != nil {
		return false, r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusProvisionFailed, err, "failed to list namespaces with label owner '%s'", username)
	}

	toDeprovision, found := nextNamespaceToDeprovision(nsTmplSet.Spec.Namespaces, userNamespaces)
	if found {
		if err := r.setStatusUpdatingIfNotProvisioning(nsTmplSet); err != nil {
			return false, err
		}
		if err := r.client.Delete(context.TODO(), toDeprovision); err != nil {
			return false, r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusUpdateFailed, err, "failed to delete namespace %s", toDeprovision.Name)
		}
		logger.Info("deleted namespace as part of NSTemplateSet update", "namespace", toDeprovision.Name)
		return true, nil // we deleted the namespace - wait for another reconcile
	}

	// find next namespace for provisioning namespace resource
	tcNamespace, userNamespace, found := nextNamespaceToProvisionOrUpdate(nsTmplSet, userNamespaces)
	if !found {
		logger.Info("no more namespaces to create", "username", nsTmplSet.GetName())
		return false, nil
	}

	if len(userNamespaces) > 0 {
		if err := r.setStatusUpdatingIfNotProvisioning(nsTmplSet); err != nil {
			return false, err
		}
	} else {
		if err := r.setStatusProvisioningIfNotUpdating(nsTmplSet); err != nil {
			return false, err
		}
	}

	// create namespace resource
	return true, r.ensureNamespace(logger, nsTmplSet, tcNamespace, userNamespace)
}

// ensureNamespace ensures that the namespace exists and that it contains all the expected resources
func (r *namespacesManager) ensureNamespace(logger logr.Logger, nsTmplSet *toolchainv1alpha1.NSTemplateSet, tcNamespace *toolchainv1alpha1.NSTemplateSetNamespace, userNamespace *corev1.Namespace) error {
	logger.Info("ensuring namespace", "namespace", tcNamespace.Type, "tier", nsTmplSet.Spec.TierName)

	// create namespace before created inner resources because creating the namespace may take some time
	if userNamespace == nil {
		return r.ensureNamespaceResource(logger, nsTmplSet, tcNamespace)
	}
	return r.ensureInnerNamespaceResources(logger, nsTmplSet, tcNamespace, userNamespace)
}

// ensureNamespaceResource ensures that the namespace exists.
func (r *namespacesManager) ensureNamespaceResource(logger logr.Logger, nsTmplSet *toolchainv1alpha1.NSTemplateSet, tcNamespace *toolchainv1alpha1.NSTemplateSetNamespace) error {
	logger.Info("creating namespace", "username", nsTmplSet.GetName(), "tier", nsTmplSet.Spec.TierName, "type", tcNamespace.Type)

	objs, err := process(r.templateContent(nsTmplSet.Spec.TierName, tcNamespace.Type), r.scheme, nsTmplSet.GetName(), template.RetainNamespaces)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to process template for namespace type '%s'", tcNamespace.Type)
	}

	labels := map[string]string{
		toolchainv1alpha1.OwnerLabelKey:    nsTmplSet.GetName(),
		toolchainv1alpha1.TypeLabelKey:     tcNamespace.Type,
		toolchainv1alpha1.ProviderLabelKey: toolchainv1alpha1.ProviderLabelValue,
	}

	// Note: we don't see an owner reference between the NSTemplateSet (namespaced resource) and the namespace (cluster-wide resource)
	// because a namespaced resource cannot be the owner of a cluster resource (the GC will delete the child resource, considering it is an orphan resource)
	// As a consequence, when the NSTemplateSet is deleted, we explicitly delete the associated namespaces that belong to the same user.
	// see https://issues.redhat.com/browse/CRT-429

	_, err = applycl.NewApplyClient(r.client, r.scheme).Apply(objs, labels)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to create namespace with type '%s'", tcNamespace.Type)
	}
	logger.Info("namespace provisioned", "namespace", tcNamespace)
	return nil
}

// ensureInnerNamespaceResources ensure that the namespace has the expected resources.
func (r *namespacesManager) ensureInnerNamespaceResources(logger logr.Logger, nsTmplSet *toolchainv1alpha1.NSTemplateSet, tcNamespace *toolchainv1alpha1.NSTemplateSetNamespace, namespace *corev1.Namespace) error {
	logger.Info("ensuring namespace resources", "username", nsTmplSet.GetName(), "tier", nsTmplSet.Spec.TierName, "type", tcNamespace.Type)
	nsName := namespace.GetName()
	username := nsTmplSet.GetName()
	newObjs, err := process(r.templateContent(nsTmplSet.Spec.TierName, tcNamespace.Type), r.scheme, username, template.RetainAllButNamespaces)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to process template for namespace '%s'", nsName)
	}

	if currentTier, exists := namespace.Labels[toolchainv1alpha1.TierLabelKey]; exists && currentTier != "" && currentTier != nsTmplSet.Spec.TierName {
		if err := r.setStatusUpdatingIfNotProvisioning(nsTmplSet); err != nil {
			return err
		}

		currentObjs, err := process(r.templateContent(currentTier, tcNamespace.Type), r.scheme, username, template.RetainAllButNamespaces)
		if err != nil {
			return r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusUpdateFailed, err, "failed to retrieve template for tier/type '%s/%s'", currentTier, tcNamespace.Type)
		}
		if _, err := deleteRedundantObjects(logger, r.client, false, currentObjs, newObjs); err != nil {
			return r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusUpdateFailed, err, "failed to delete redundant objects in namespace '%s'", nsName)
		}
	}

	var labels = map[string]string{
		toolchainv1alpha1.ProviderLabelKey: toolchainv1alpha1.ProviderLabelValue,
	}
	if _, err = applycl.NewApplyClient(r.client, r.scheme).Apply(newObjs, labels); err != nil {
		return r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to provision namespace '%s' with required resources", nsName)
	}

	if namespace.Labels == nil {
		namespace.Labels = make(map[string]string)
	}

	// Adding labels indicating that the namespace is up-to-date with revision/tier
	namespace.Labels[toolchainv1alpha1.RevisionLabelKey] = tcNamespace.Revision
	namespace.Labels[toolchainv1alpha1.TierLabelKey] = nsTmplSet.Spec.TierName
	if err := r.client.Update(context.TODO(), namespace); err != nil {
		return r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to update namespace '%s'", nsName)
	}

	logger.Info("namespace provisioned with all required resources", "tier", nsTmplSet.Spec.TierName, "namespace", tcNamespace)

	// TODO add validation for other objects
	return nil // nothing changed, no error occurred
}

// delete deletes namespaces that are owned by the user (based on the label). The method deletes only one namespace in one call
// and returns information if any namespace was deleted or not. The cases are described below:
//
// If there is still some namespace owned by the user, then it deletes it and returns 'true,nil'. If there is no namespace found
// which means that everything was deleted previously, then it returns 'false,nil'. In case of any error it returns 'false,error'.
func (r *namespacesManager) delete(logger logr.Logger, nsTmplSet *toolchainv1alpha1.NSTemplateSet) (bool, error) {
	// now, we can delete all "child" namespaces explicitly
	username := nsTmplSet.Name
	userNamespaces, err := fetchNamespaces(r.client, username)
	if err != nil {
		return false, r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusTerminatingFailed, err, "failed to list namespace with label owner '%s'", username)
	}
	// delete the first namespace which (still) exists and is not in a terminating state
	logger.Info("checking user namepaces associated with the deleted NSTemplateSet...")
	for _, ns := range userNamespaces {
		if !util.IsBeingDeleted(&ns) {
			logger.Info("deleting a user namepace associated with the deleted NSTemplateSet", "namespace", ns.Name)
			if err := r.client.Delete(context.TODO(), &ns); err != nil {
				return false, r.wrapErrorWithStatusUpdate(logger, nsTmplSet, r.setStatusTerminatingFailed, err, "failed to delete user namespace '%s'", ns.Name)
			}
			return true, nil
		}
	}
	return false, nil
}

// fetchNamespaces returns all current namespaces belonging to the given user
// i.e., labeled with `"toolchain.dev.openshift.com/owner":<username>`
func fetchNamespaces(client client.Client, username string) ([]corev1.Namespace, error) {
	// fetch all namespace with owner=username label
	userNamespaceList := &corev1.NamespaceList{}
	if err := client.List(context.TODO(), userNamespaceList, listByOwnerLabel(username)); err != nil {
		return nil, err
	}
	return userNamespaceList.Items, nil
}

// nextNamespaceToProvisionOrUpdate returns first namespace (from given namespaces) whose status is active and
// either revision is not set or revision or tier doesn't equal to the current one.
// It also returns namespace present in tcNamespaces but not found in given namespaces
func nextNamespaceToProvisionOrUpdate(nsTmplSet *toolchainv1alpha1.NSTemplateSet, namespaces []corev1.Namespace) (*toolchainv1alpha1.NSTemplateSetNamespace, *corev1.Namespace, bool) {
	for _, tcNamespace := range nsTmplSet.Spec.Namespaces {
		namespace, found := findNamespace(namespaces, tcNamespace.Type)
		if found {
			if namespace.Status.Phase == corev1.NamespaceActive {
				if namespace.Labels[toolchainv1alpha1.RevisionLabelKey] == "" ||
					namespace.Labels[toolchainv1alpha1.RevisionLabelKey] != tcNamespace.Revision ||
					namespace.Labels[toolchainv1alpha1.TierLabelKey] != nsTmplSet.Spec.TierName {
					return &tcNamespace, &namespace, true
				}
			}
		} else {
			return &tcNamespace, nil, true
		}
	}
	return nil, nil, false
}

// nextNamespaceToDeprovision returns namespace (and information of it was found) that should be deprovisioned
// because its type wasn't found in the set of namespace types in NSTemplateSet
func nextNamespaceToDeprovision(tcNamespaces []toolchainv1alpha1.NSTemplateSetNamespace, namespaces []corev1.Namespace) (*corev1.Namespace, bool) {
Namespaces:
	for _, ns := range namespaces {
		for _, tcNs := range tcNamespaces {
			if tcNs.Type == ns.Labels[toolchainv1alpha1.TypeLabelKey] {
				continue Namespaces
			}
		}
		return &ns, true
	}
	return nil, false
}

func findNamespace(namespaces []corev1.Namespace, typeName string) (corev1.Namespace, bool) {
	for _, ns := range namespaces {
		if ns.Labels[toolchainv1alpha1.TypeLabelKey] == typeName {
			return ns, true
		}
	}
	return corev1.Namespace{}, false
}

func getNamespaceName(request reconcile.Request) (string, error) {
	namespace := request.Namespace
	if namespace == "" {
		return k8sutil.GetWatchNamespace()
	}
	return namespace, nil
}
