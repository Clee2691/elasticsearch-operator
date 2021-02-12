package indexmanagement

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	batch "k8s.io/api/batch/v1beta1"
	core "k8s.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apis "github.com/openshift/elasticsearch-operator/pkg/apis/logging/v1"
	"github.com/openshift/elasticsearch-operator/pkg/constants"
	"github.com/openshift/elasticsearch-operator/pkg/log"
	kverrors "github.com/openshift/elasticsearch-operator/pkg/log"
	"github.com/openshift/elasticsearch-operator/pkg/logger"
	k8s "github.com/openshift/elasticsearch-operator/pkg/types/k8s"
	"github.com/openshift/elasticsearch-operator/pkg/utils"
	"github.com/openshift/elasticsearch-operator/pkg/utils/comparators"
)

const (
	indexManagementConfigmap = "indexmanagement-scripts"
	defaultShardSize         = int32(40)
	workingDir               = "/tmp/scripts"
)

var (
	defaultCPURequest      = resource.MustParse("100m")
	defaultMemoryRequest   = resource.MustParse("32Mi")
	jobHistoryLimitFailed  = utils.GetInt32(1)
	jobHistoryLimitSuccess = utils.GetInt32(1)

	millisPerSecond = uint64(1000)
	millisPerMinute = uint64(60 * millisPerSecond)
	millisPerHour   = uint64(millisPerMinute * 60)
	millisPerDay    = uint64(millisPerHour * 24)
	millisPerWeek   = uint64(millisPerDay * 7)

	//fullExecMode 0777
	fullExecMode = utils.GetInt32(int32(511))

	imLabels = map[string]string{
		"provider":      "openshift",
		"component":     "indexManagement",
		"logging-infra": "indexManagement",
	}
)

type rolloverConditions struct {
	MaxAge  string `json:"max_age,omitempty"`
	MaxDocs int32  `json:"max_docs,omitempty"`
	MaxSize string `json:"max_size,omitempty"`
}

func RemoveCronJobsForMappings(apiclient client.Client, cluster *apis.Elasticsearch, mappings []apis.IndexManagementPolicyMappingSpec, policies apis.PolicyMap) error {
	expected := sets.NewString()
	for _, mapping := range mappings {
		expected.Insert(fmt.Sprintf("%s-im-%s", cluster.Name, mapping.Name))
	}
	logger.Debugf("Expecting to have cronjobs in %s: %v", cluster.Namespace, expected.List())

	labelSelector := labels.SelectorFromSet(imLabels)

	cronList := &batch.CronJobList{}
	if err := apiclient.List(context.TODO(), &client.ListOptions{Namespace: cluster.Namespace, LabelSelector: labelSelector}, cronList); err != nil {
		return err
	}
	existing := sets.NewString()
	for _, cron := range cronList.Items {
		existing.Insert(cron.Name)
	}
	difference := existing.Difference(expected)
	logger.Debugf("Removing cronjobs in %s: %v", cluster.Namespace, difference.List())
	for _, name := range difference.List() {
		cronjob := &batch.CronJob{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CronJob",
				APIVersion: batch.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
			},
		}
		err := apiclient.Delete(context.TODO(), cronjob)
		if err != nil && !errors.IsNotFound(err) {
			logger.Errorf("Failure culling %s/%s cronjob %v", cluster.Namespace, name, err)
		}
	}
	return nil
}

func ReconcileCurationConfigmap(apiclient client.Client, cluster *apis.Elasticsearch) error {
	data := scriptMap
	desired := k8s.NewConfigMap(indexManagementConfigmap, cluster.Namespace, imLabels, data)
	cluster.AddOwnerRefTo(desired)
	err := apiclient.Create(context.TODO(), desired)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("Error creating configmap for cluster %s: %v", cluster.Name, err)
		}
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &v1.ConfigMap{}
			retryError := apiclient.Get(context.TODO(), types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
			if retryError != nil {
				return fmt.Errorf("Unable to get configmap %s/%s during reconciliation: %v", desired.Namespace, desired.Name, retryError)
			}
			if !reflect.DeepEqual(desired.Data, current.Data) {
				logger.Debugf("Updating configmap %s/%s", current.Namespace, current.Name)
				current.Data = desired.Data
				return apiclient.Update(context.TODO(), current)
			}
			return nil
		})
	}
	return err
}

func ReconcileIndexManagementCronjob(apiclient client.Client, cluster *apis.Elasticsearch, policy apis.IndexManagementPolicySpec, mapping apis.IndexManagementPolicyMappingSpec, primaryShards int32) error {
	if policy.Phases.Delete == nil && policy.Phases.Hot == nil {
		log.V(1).Info("Skipping indexmanagement cronjob for policymapping; no phases are defined", "policymapping", mapping.Name)
		return nil
	}
	envvars := []corev1.EnvVar{}
	if policy.Phases.Delete != nil {
		minAgeMillis, err := calculateMillisForTimeUnit(policy.Phases.Delete.MinAge)
		if err != nil {
			return err
		}
		envvars = append(envvars,
			corev1.EnvVar{Name: "POLICY_MAPPING", Value: mapping.Name},
			corev1.EnvVar{Name: "MIN_AGE", Value: strconv.FormatUint(minAgeMillis, 10)},
		)
	} else {
		log.V(1).Info("Skipping curation management for policymapping; delete phase not defined", "policymapping", mapping.Name)
	}

	if policy.Phases.Hot != nil {
		conditions := calculateConditions(policy, primaryShards)
		payload, err := json.Marshal(map[string]rolloverConditions{"conditions": conditions})
		if err != nil {
			return kverrors.Wrap(err, "failed to serialize the rollover conditions to JSON")
		}
		envvars = append(envvars,
			corev1.EnvVar{Name: "PAYLOAD", Value: base64.StdEncoding.EncodeToString(payload)},
			corev1.EnvVar{Name: "POLICY_MAPPING", Value: mapping.Name},
		)

	} else {
		log.V(1).Info("Skipping rollover management for policymapping; hot phase not defined", "policymapping", mapping.Name)
	}
	schedule, err := crontabScheduleFor(policy.PollInterval)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-im-%s", cluster.Name, mapping.Name)
	script := formatCmd(policy)
	desired := newCronJob(cluster.Name, cluster.Namespace, name, schedule, script, cluster.Spec.Spec.NodeSelector, cluster.Spec.Spec.Tolerations, envvars)

	cluster.AddOwnerRefTo(desired)
	return reconcileCronJob(apiclient, cluster, desired, areCronJobsSame)
}

func formatCmd(policy apis.IndexManagementPolicySpec) string {
	cmd := []string{}
	result := []string{}
	if policy.Phases.Delete != nil {
		cmd = append(cmd, "./delete", "delete_rc=$?")
		result = append(result, "exit $delete_rc")
	}
	if policy.Phases.Hot != nil {
		cmd = append(cmd, "./rollover", "rollover_rc=$?")
		result = append(result, "exit $rollover_rc")
	}
	if len(cmd) == 0 {
		return ""
	}
	cmd = append(cmd, fmt.Sprintf("$(%s)", strings.Join(result, "&&")))
	script := strings.Join(cmd, ";")
	return script
}

func reconcileCronJob(apiclient client.Client, cluster *apis.Elasticsearch, desired *batch.CronJob, fnAreCronJobsSame func(lhs, rhs *batch.CronJob) bool) error {
	err := apiclient.Create(context.TODO(), desired)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("Error creating cronjob for cluster %s: %v", cluster.Name, err)
		}
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current := &batch.CronJob{}
			retryError := apiclient.Get(context.TODO(), types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
			if retryError != nil {
				return fmt.Errorf("Unable to get cronjob %s/%s during reconciliation: %v", desired.Namespace, desired.Name, retryError)
			}
			if !fnAreCronJobsSame(current, desired) {
				current.Spec = desired.Spec
				return apiclient.Update(context.TODO(), current)
			}
			return nil
		})
	}
	return err
}

func areCronJobsSame(lhs, rhs *batch.CronJob) bool {
	logger.Debugf("Evaluating cronjob '%s/%s' ...", lhs.Namespace, lhs.Name)
	if len(lhs.Spec.JobTemplate.Spec.Template.Spec.Containers) != len(lhs.Spec.JobTemplate.Spec.Template.Spec.Containers) {
		logger.Debugf("Container lengths are different between current and desired for %s/%s", lhs.Namespace, lhs.Name)
		return false
	}
	if !comparators.AreStringMapsSame(lhs.Spec.JobTemplate.Spec.Template.Spec.NodeSelector, rhs.Spec.JobTemplate.Spec.Template.Spec.NodeSelector) {
		logger.Debugf("NodeSelector is different between current and desired for %s/%s", lhs.Namespace, lhs.Name)
		return false
	}

	if !comparators.AreTolerationsSame(lhs.Spec.JobTemplate.Spec.Template.Spec.Tolerations, rhs.Spec.JobTemplate.Spec.Template.Spec.Tolerations) {
		logger.Debugf("Tolerations are different between current and desired for %s/%s", lhs.Namespace, lhs.Name)
		return false
	}
	if lhs.Spec.Schedule != rhs.Spec.Schedule {
		logger.Debugf("Schedule is different between current and desired for %s/%s", lhs.Namespace, lhs.Name)
		lhs.Spec.Schedule = rhs.Spec.Schedule
		return false
	}
	if lhs.Spec.Suspend != nil && rhs.Spec.Suspend != nil && *lhs.Spec.Suspend != *rhs.Spec.Suspend {
		logger.Debugf("Suspend is different between current and desired for %s/%s", lhs.Namespace, lhs.Name)
		return false
	}
	for i, container := range lhs.Spec.JobTemplate.Spec.Template.Spec.Containers {
		logger.Debugf("Evaluating cronjob container %q ...", container.Name)
		other := rhs.Spec.JobTemplate.Spec.Template.Spec.Containers[i]
		if !areContainersSame(container, other) {
			return false
		}
	}
	return true
}

func areContainersSame(container, other corev1.Container) bool {
	if container.Name != other.Name {
		return false
	}
	if container.Image != other.Image {
		return false
	}

	if !reflect.DeepEqual(container.Command, other.Command) {
		return false
	}
	if !reflect.DeepEqual(container.Args, other.Args) {
		return false
	}

	if !comparators.AreResourceRequementsSame(container.Resources, other.Resources) {
		return false
	}

	if !comparators.EnvValueEqual(container.Env, other.Env) {
		return false
	}
	logger.Debug("The current and desired cronjobs are the same")
	return true
}

func newContainer(clusterName, name, image, scriptPath string, envvars []corev1.EnvVar) corev1.Container {
	envvars = append(envvars, corev1.EnvVar{Name: "ES_SERVICE", Value: fmt.Sprintf("https://%s:9200", clusterName)})
	container := corev1.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: core.PullIfNotPresent,
		Resources: v1.ResourceRequirements{
			Requests: v1.ResourceList{
				v1.ResourceMemory: defaultMemoryRequest,
				v1.ResourceCPU:    defaultCPURequest,
			},
		},
		WorkingDir: workingDir,
		Env:        envvars,
		Command:    []string{"bash"},
		Args: []string{
			"-c",
			scriptPath,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "certs", ReadOnly: true, MountPath: "/etc/indexmanagement/keys"},
			{Name: "scripts", ReadOnly: false, MountPath: workingDir},
		},
	}

	return container
}

func newCronJob(clusterName, namespace, name, schedule, script string, nodeSelector map[string]string, tolerations []corev1.Toleration, envvars []corev1.EnvVar) *batch.CronJob {
	containerName := "indexmanagement"
	podSpec := corev1.PodSpec{
		ServiceAccountName: clusterName,
		Containers:         []corev1.Container{newContainer(clusterName, containerName, constants.PackagedElasticsearchImage(), script, envvars)},
		Volumes: []corev1.Volume{
			{Name: "certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: clusterName}}},
			{Name: "scripts", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: indexManagementConfigmap}, DefaultMode: fullExecMode}}},
		},
		NodeSelector:                  utils.EnsureLinuxNodeSelector(nodeSelector),
		Tolerations:                   tolerations,
		RestartPolicy:                 v1.RestartPolicyNever,
		TerminationGracePeriodSeconds: utils.GetInt64(300),
	}
	cronJob := &batch.CronJob{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CronJob",
			APIVersion: batch.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    imLabels,
		},
		Spec: batch.CronJobSpec{
			ConcurrencyPolicy:          batch.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: jobHistoryLimitSuccess,
			FailedJobsHistoryLimit:     jobHistoryLimitFailed,
			Schedule:                   schedule,
			JobTemplate: batch.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit: utils.GetInt32(0),
					Parallelism:  utils.GetInt32(1),
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      containerName,
							Namespace: namespace,
							Labels:    imLabels,
						},
						Spec: podSpec,
					},
				},
			},
		},
	}

	return cronJob
}
