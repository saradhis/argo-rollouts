package controller

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	patchtypes "k8s.io/apimachinery/pkg/types"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/argoproj/argo-rollouts/utils/annotations"
	logutil "github.com/argoproj/argo-rollouts/utils/log"
)

const (
	switchSelectorPatch = `{
	"spec": {
		"selector": {
			"%s": "%s"
		}
	}
}
`
)

// switchSelector switch the selector on an existing service to a new value
func (c Controller) switchServiceSelector(service *corev1.Service, newRolloutUniqueLabelValue string, r *v1alpha1.Rollout) error {
	patch := fmt.Sprintf(switchSelectorPatch, v1alpha1.DefaultRolloutUniqueLabelKey, newRolloutUniqueLabelValue)
	logutil.WithRollout(r).Infof("Switching selector for service '%s' to value '%s'", service.Name, newRolloutUniqueLabelValue)
	_, err := c.kubeclientset.CoreV1().Services(service.Namespace).Patch(service.Name, patchtypes.StrategicMergePatchType, []byte(patch))
	return err
}

func (c *Controller) reconcilePreviewService(r *v1alpha1.Rollout, newRS *appsv1.ReplicaSet, previewSvc *corev1.Service, activeSvc *corev1.Service) (bool, error) {
	if previewSvc == nil {
		return false, nil
	}
	if !annotations.IsSaturated(r, newRS) {
		logutil.WithRollout(r).Infof("New RS '%s' is not fully saturated", newRS.Name)
		return true, nil
	}

	//If the active service selector does not point to any RS,
	// we short-circuit changing the preview service.
	if activeSvc.Spec.Selector == nil {
		return false, nil
	}
	// If the active service selector points at the new RS, the
	// preview service should point at nothing
	curActiveSelector, ok := activeSvc.Spec.Selector[v1alpha1.DefaultRolloutUniqueLabelKey]
	if !ok || curActiveSelector == newRS.Labels[v1alpha1.DefaultRolloutUniqueLabelKey] {
		curPreviewSelector, ok := previewSvc.Spec.Selector[v1alpha1.DefaultRolloutUniqueLabelKey]
		if !ok || curPreviewSelector != "" {
			err := c.switchServiceSelector(previewSvc, "", r)
			if err != nil {
				return false, err
			}
		}
		return false, nil
	}

	// If preview service already points to the new RS, skip the next steps
	if previewSvc.Spec.Selector != nil {
		currentSelectorValue, ok := previewSvc.Spec.Selector[v1alpha1.DefaultRolloutUniqueLabelKey]
		if ok && currentSelectorValue == newRS.Labels[v1alpha1.DefaultRolloutUniqueLabelKey] {
			return false, nil
		}
	}

	err := c.setVerifyingPreview(r)
	if err != nil {
		return false, err
	}

	err = c.switchServiceSelector(previewSvc, newRS.Labels[v1alpha1.DefaultRolloutUniqueLabelKey], r)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (c *Controller) reconcileActiveService(r *v1alpha1.Rollout, newRS *appsv1.ReplicaSet, previewSvc *corev1.Service, activeSvc *corev1.Service) (bool, error) {
	if !annotations.IsSaturated(r, newRS) {
		return false, nil
	}

	switchActiveSvc := true
	if activeSvc.Spec.Selector != nil {
		currentSelectorValue, ok := activeSvc.Spec.Selector[v1alpha1.DefaultRolloutUniqueLabelKey]
		if ok && currentSelectorValue == newRS.Labels[v1alpha1.DefaultRolloutUniqueLabelKey] {
			switchActiveSvc = false
		}
	}
	if switchActiveSvc {
		err := c.switchServiceSelector(activeSvc, newRS.Labels[v1alpha1.DefaultRolloutUniqueLabelKey], r)
		if err != nil {
			return false, err
		}
		return true, nil
	}

	switchPreviewSvc := false
	if previewSvc != nil && previewSvc.Spec.Selector != nil {
		currentSelectorValue, ok := previewSvc.Spec.Selector[v1alpha1.DefaultRolloutUniqueLabelKey]
		if !ok || currentSelectorValue != "" {
			switchPreviewSvc = true
		}
	}

	if switchPreviewSvc {
		err := c.switchServiceSelector(previewSvc, "", r)
		if err != nil {
			return false, err
		}
		return true, nil
	}

	return false, nil
}

func (c *Controller) getRolloutsForService(service *corev1.Service) ([]*v1alpha1.Rollout, error) {
	allROs, err := c.rolloutsclientset.ArgoprojV1alpha1().Rollouts(service.Namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	rollouts := []*v1alpha1.Rollout{}
	for _, rollout := range allROs.Items {
		if rollout.Spec.Strategy.BlueGreenStrategy.ActiveService == service.Name {
			copyRO := rollout.DeepCopy()
			rollouts = append(rollouts, copyRO)
		}
	}
	if len(rollouts) > 1 {
		errMessage := fmt.Sprintf("More tha one rollout is selecting replica set %s/%s with labels: %#v",
			service.Namespace, service.Name, service.Labels)
		log.Error(errMessage)
		return nil, fmt.Errorf(errMessage)
	}
	return rollouts, nil
}

func (c *Controller) handleService(obj interface{}) {
	service := obj.(*corev1.Service)
	rollouts, err := c.getRolloutsForService(service)
	if err != nil {
		return
	}
	if len(rollouts) != 1 {
		return
	}
	c.enqueueRollout(rollouts[0])
}

func (c *Controller) updateService(old, cur interface{}) {
	curSvc := cur.(*corev1.Service)
	oldSvc := old.(*corev1.Service)
	if curSvc.ResourceVersion == oldSvc.ResourceVersion {
		// Periodic resync will send update events for all known services.
		// Two different versions of the same replica set will always have different RVs.
		return
	}
	c.handleService(cur)
}

func (c *Controller) getPreviewAndActiveServices(r *v1alpha1.Rollout) (*corev1.Service, *corev1.Service, error) {
	var previewSvc *corev1.Service
	var activeSvc *corev1.Service
	var err error
	logCtx := logutil.WithRollout(r)
	if r.Spec.Strategy.BlueGreenStrategy.PreviewService != "" {
		previewSvc, err = c.kubeclientset.CoreV1().Services(r.Namespace).Get(r.Spec.Strategy.BlueGreenStrategy.PreviewService, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				logCtx.Warnf("Service %v does not exist", r.Spec.Strategy.BlueGreenStrategy.PreviewService)
			}
			return nil, nil, err
		}
	}
	if r.Spec.Strategy.BlueGreenStrategy.ActiveService == "" {
		return nil, nil, fmt.Errorf("Invalid Spec: Rollout missing field ActiveService")
	}
	activeSvc, err = c.kubeclientset.CoreV1().Services(r.Namespace).Get(r.Spec.Strategy.BlueGreenStrategy.ActiveService, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			logCtx.Warnf("Service %v does not exist", r.Spec.Strategy.BlueGreenStrategy.PreviewService)
		}
		return nil, nil, err
	}
	return previewSvc, activeSvc, nil
}

func (c *Controller) getRolloutSelectorLabel(svc *corev1.Service) (string, bool) {
	if svc == nil {
		return "", false
	}
	if svc.Spec.Selector == nil {
		return "", false
	}
	currentSelectorValue, ok := svc.Spec.Selector[v1alpha1.DefaultRolloutUniqueLabelKey]
	return currentSelectorValue, ok
}

// GetActiveReplicaSet finds the replicaset that is serving traffic from the active service or returns nil
func GetActiveReplicaSet(rollout *v1alpha1.Rollout, allRS []*appsv1.ReplicaSet) *appsv1.ReplicaSet {
	if rollout.Status.ActiveSelector == "" {
		return nil
	}
	for _, rs := range allRS {
		if rs == nil {
			continue
		}
		if podHash, ok := rs.Labels[v1alpha1.DefaultRolloutUniqueLabelKey]; ok {
			if podHash == rollout.Status.ActiveSelector {
				return rs
			}
		}
	}
	return nil
}
