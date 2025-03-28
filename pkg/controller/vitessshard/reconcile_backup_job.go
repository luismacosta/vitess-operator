/*
Copyright 2019 PlanetScale Inc.

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

package vitessshard

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	planetscalev2 "planetscale.dev/vitess-operator/pkg/apis/planetscale/v2"
	"planetscale.dev/vitess-operator/pkg/operator/reconciler"
	"planetscale.dev/vitess-operator/pkg/operator/results"
	"planetscale.dev/vitess-operator/pkg/operator/update"
	"planetscale.dev/vitess-operator/pkg/operator/vitessbackup"
	"planetscale.dev/vitess-operator/pkg/operator/vttablet"
)

func (r *ReconcileVitessShard) reconcileBackupJob(ctx context.Context, vts *planetscalev2.VitessShard) (reconcile.Result, error) {
	resultBuilder := &results.Builder{}

	// Break early if we find we are using an externally managed MySQL, or if any tablet pools have nil for Mysqld,
	// because we should not be configuring backups in either case.
	if vts.Spec.UsingExternalDatastore() || !vts.Spec.AllPoolsUsingMysqld() {
		return resultBuilder.Result()
	}

	clusterName := vts.Labels[planetscalev2.ClusterLabel]
	keyspaceName := vts.Labels[planetscalev2.KeyspaceLabel]
	shardSafeName := vts.Spec.KeyRange.SafeName()

	labels := map[string]string{
		planetscalev2.ComponentLabel: planetscalev2.VtbackupComponentName,
		planetscalev2.ClusterLabel:   clusterName,
		planetscalev2.KeyspaceLabel:  keyspaceName,
		planetscalev2.ShardLabel:     shardSafeName,
		vitessbackup.TypeLabel:       vitessbackup.TypeInit,
	}

	allBackups, completeBackups, err := vitessbackup.GetBackups(ctx, vts.Namespace, clusterName, keyspaceName, shardSafeName,
		func(ctx context.Context, allBackupsList *planetscalev2.VitessBackupList, listOpts *client.ListOptions) error {
			return r.client.List(ctx, allBackupsList, listOpts)
		},
	)
	if err != nil {
		return resultBuilder.Error(err)
	}
	updateBackupStatus(vts, allBackups)

	// Generate keys (object names) for all desired backup Pods and PVCs.
	// Keep a map back from generated names to the backup specs.
	podKeys := []client.ObjectKey{}
	pvcKeys := []client.ObjectKey{}
	specMap := map[client.ObjectKey]*vttablet.BackupSpec{}

	// The object name for the initial backup Pod, if we end up needing one.
	initPodName := vttablet.InitialBackupPodName(clusterName, keyspaceName, vts.Spec.KeyRange)
	initPodKey := client.ObjectKey{
		Namespace: vts.Namespace,
		Name:      initPodName,
	}

	if len(completeBackups) == 0 && vts.Status.HasMaster != corev1.ConditionTrue {
		// Until we see at least one complete backup, we attempt to create an
		// "initial backup", which is a special imaginary backup created from
		// scratch (not from any tablet). If we're wrong and a backup exists
		// already, the idempotent vtbackup "initial backup" mode will just do
		// nothing and return success.
		initSpec := MakeVtbackupSpec(initPodKey, vts, labels, vitessbackup.TypeInit)
		if initSpec != nil {
			podKeys = append(podKeys, initPodKey)
			if initSpec.TabletSpec.DataVolumePVCSpec != nil {
				pvcKeys = append(pvcKeys, initPodKey)
			}
			specMap[initPodKey] = initSpec
		}
	} else {
		// We have at least one complete backup already.
		vts.Status.HasInitialBackup = corev1.ConditionTrue
	}

	// Reconcile vtbackup PVCs. Use the same key as the corresponding Pod,
	// but only if the Pod expects a PVC.
	err = r.reconciler.ReconcileObjectSet(ctx, vts, pvcKeys, labels, reconciler.Strategy{
		Kind: &corev1.PersistentVolumeClaim{},

		New: func(key client.ObjectKey) runtime.Object {
			return vttablet.NewPVC(key, specMap[key].TabletSpec)
		},
		PrepareForTurndown: func(key client.ObjectKey, obj runtime.Object) *planetscalev2.OrphanStatus {
			// Same as reconcileTablets, keep PVCs of Pods in any Phase
			pod := &corev1.Pod{}
			if getErr := r.client.Get(ctx, key, pod); getErr == nil || !apierrors.IsNotFound(getErr) {
				// If the get was successful, the Pod exists and we shouldn't delete the PVC.
				// If the get failed for any reason other than NotFound, we don't know if it's safe.
				return &planetscalev2.OrphanStatus{
					Reason:  "BackupRunning",
					Message: "Not deleting vtbackup PVC because vtbackup Pod still exists",
				}
			}
			return nil
		},
	})
	if err != nil {
		resultBuilder.Error(err)
	}

	// Reconcile vtbackup Pods.
	err = r.reconciler.ReconcileObjectSet(ctx, vts, podKeys, labels, reconciler.Strategy{
		Kind: &corev1.Pod{},

		New: func(key client.ObjectKey) runtime.Object {
			return vttablet.NewBackupPod(key, specMap[key], vts.Spec.Images.Mysqld.Image())
		},
		Status: func(key client.ObjectKey, obj runtime.Object) {
			pod := obj.(*corev1.Pod)

			// If this status hook is telling us about the special init Pod,
			// we can update HasInitialBackup.
			if key == initPodKey {
				// If the Pod is Suceeded or Failed, we can update status.
				// Otherwise, we leave it as Unknown since we can't tell.
				switch pod.Status.Phase {
				case corev1.PodSucceeded:
					vts.Status.HasInitialBackup = corev1.ConditionTrue
				case corev1.PodFailed:
					vts.Status.HasInitialBackup = corev1.ConditionFalse
				}
			}
		},
		PrepareForTurndown: func(key client.ObjectKey, obj runtime.Object) *planetscalev2.OrphanStatus {
			// As soon as the new backup is complete, the backup policy logic
			// will say the vtbackup Pod is no longer needed. However, we still
			// need to give it a chance to finish running because it does
			// pruning of old backups after the new backup is complete.
			pod := obj.(*corev1.Pod)
			if pod.Status.Phase == corev1.PodRunning {
				return &planetscalev2.OrphanStatus{
					Reason:  "BackupRunning",
					Message: "Not deleting vtbackup Pod while it's still running",
				}
			}
			return nil
		},
	})
	if err != nil {
		resultBuilder.Error(err)
	}

	return resultBuilder.Result()
}

func MakeVtbackupSpec(key client.ObjectKey, vts *planetscalev2.VitessShard, parentLabels map[string]string, typ string) *vttablet.BackupSpec {
	// If we specifically set our cluster to avoid initial backups, bail early.
	if !*vts.Spec.Replication.InitializeBackup {
		return nil
	}

	if len(vts.Spec.TabletPools) == 0 {
		// No tablet pools are defined for this shard.
		// We don't know enough to make a vtbackup spec.
		return nil
	}

	// Make a vtbackup spec that's a similar shape to the first tablet pool.
	// This should give it enough resources to run mysqld and restore a backup,
	// since all tablets need to be able to do that, regardless of type.
	return vtbackupSpec(key, vts, parentLabels, &vts.Spec.TabletPools[0], typ)
}

func vtbackupSpec(key client.ObjectKey, vts *planetscalev2.VitessShard, parentLabels map[string]string, pool *planetscalev2.VitessShardTabletPool, backupType string) *vttablet.BackupSpec {
	keyspaceName := vts.Labels[planetscalev2.KeyspaceLabel]

	// Find the backup location for this pool.
	backupLocation := vts.Spec.BackupLocation(pool.BackupLocationName)
	if backupLocation == nil {
		// No backup location is configured, so we can't do anything.
		return nil
	}

	// Copy parent labels map and add child-specific labels.
	labels := map[string]string{
		vitessbackup.LocationLabel: backupLocation.Name,
		vitessbackup.TypeLabel:     backupType,
	}
	for k, v := range parentLabels {
		labels[k] = v
	}

	minBackupInterval := time.Duration(0)
	minRetentionTime := time.Duration(0)
	minRetentionCount := 1

	// Allocate a new map so we don't mutate inputs.
	annotations := map[string]string{}
	update.Annotations(&annotations, pool.Annotations)
	update.Annotations(&annotations, backupLocation.Annotations)

	// Fill in the parts of a vttablet spec that make sense for vtbackup.
	tabletSpec := &vttablet.Spec{
		GlobalLockserver:         vts.Spec.GlobalLockserver,
		Labels:                   labels,
		Images:                   vts.Spec.Images,
		KeyRange:                 vts.Spec.KeyRange,
		Vttablet:                 &pool.Vttablet,
		Mysqld:                   pool.Mysqld,
		MysqldExporter:           pool.MysqldExporter,
		DataVolumePVCName:        key.Name,
		DataVolumePVCSpec:        pool.DataVolumeClaimTemplate,
		KeyspaceName:             keyspaceName,
		DatabaseName:             vts.Spec.DatabaseName,
		DatabaseInitScriptSecret: vts.Spec.DatabaseInitScriptSecret,
		BackupLocation:           backupLocation,
		BackupEngine:             vts.Spec.BackupEngine,
		InitContainers:           pool.InitContainers,
		SidecarContainers:        pool.SidecarContainers,
		ExtraEnv:                 pool.ExtraEnv,
		Annotations:              annotations,
		Affinity:                 pool.Affinity,
		Tolerations:              pool.Tolerations,
		ImagePullSecrets:         vts.Spec.ImagePullSecrets,
	}

	return &vttablet.BackupSpec{
		InitialBackup:     backupType == vitessbackup.TypeInit,
		MinBackupInterval: minBackupInterval,
		MinRetentionTime:  minRetentionTime,
		MinRetentionCount: minRetentionCount,

		TabletSpec: tabletSpec,
	}
}

func updateBackupStatus(vts *planetscalev2.VitessShard, allBackups []planetscalev2.VitessBackup) {
	// If no backup locations are configured, there's nothing to do.
	if len(vts.Spec.BackupLocations) == 0 {
		return
	}

	// Initialize status for each backup location.
	locationStatus := map[string]*planetscalev2.ShardBackupLocationStatus{}
	for i := range vts.Spec.BackupLocations {
		location := &vts.Spec.BackupLocations[i]
		status := planetscalev2.NewShardBackupLocationStatus(location.Name)
		locationStatus[location.Name] = status
		vts.Status.BackupLocations = append(vts.Status.BackupLocations, status)
	}

	// Report stats on backups, grouped by location.
	for i := range allBackups {
		backup := &allBackups[i]
		locationName := backup.Labels[vitessbackup.LocationLabel]
		location := locationStatus[locationName]
		if location == nil {
			// This is not one of the locations we care about.
			continue
		}

		if backup.Status.Complete {
			location.CompleteBackups++

			if location.LatestCompleteBackupTime == nil || backup.Status.StartTime.After(location.LatestCompleteBackupTime.Time) {
				location.LatestCompleteBackupTime = &backup.Status.StartTime
			}
		} else {
			location.IncompleteBackups++
		}
	}
}
