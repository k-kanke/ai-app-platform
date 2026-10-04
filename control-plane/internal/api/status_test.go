package api_test

import appsv1 "k8s.io/api/apps/v1"

func appsStatus(gen int64) appsv1.DeploymentStatus {
	return appsv1.DeploymentStatus{ObservedGeneration: gen, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
}
