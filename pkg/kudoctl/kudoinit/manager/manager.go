package manager

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/kudobuilder/kudo/pkg/engine/health"
	"github.com/kudobuilder/kudo/pkg/kudoctl/clog"
	"github.com/kudobuilder/kudo/pkg/kudoctl/kube"
	"github.com/kudobuilder/kudo/pkg/kudoctl/kudoinit"
	"github.com/kudobuilder/kudo/pkg/kudoctl/verifier"
)

// Ensure kudoinit.Step is implemented
var _ kudoinit.Step = &Initializer{}

//Defines the deployment of the KUDO manager and it's service definition.
type Initializer struct {
	options    kudoinit.Options
	service    *corev1.Service
	deployment *appsv1.StatefulSet
}

// NewInitializer returns the setup management object
func NewInitializer(options kudoinit.Options) Initializer {
	return Initializer{
		options:    options,
		service:    generateService(options),
		deployment: generateDeployment(options),
	}
}

func (m Initializer) PreInstallVerify(client *kube.Client, result *verifier.Result) error {
	return m.verifyManagerNotInstalled(client, result)
}

func (m Initializer) PreUpgradeVerify(client *kube.Client, result *verifier.Result) error {
	// For upgrade we don't verify anything. We assume the install process can overwrite existing manager
	return nil
}

func (m Initializer) VerifyInstallation(client *kube.Client, result *verifier.Result) error {
	return m.verifyManagerInstalled(client, result)
}

func (m Initializer) String() string {
	return "kudo controller"
}

// Install uses Kubernetes client to install KUDO.
func (m Initializer) Install(client *kube.Client) error {
	if err := m.installStatefulSet(client); err != nil {
		return err
	}
	if err := m.installService(client.KubeClient.CoreV1()); err != nil {
		return err
	}
	return nil
}

func UninstallStatefulSet(client *kube.Client, options kudoinit.Options) error {
	fg := metav1.DeletePropagationForeground
	err := client.KubeClient.AppsV1().StatefulSets(options.Namespace).Delete(context.TODO(), kudoinit.DefaultManagerName, metav1.DeleteOptions{
		PropagationPolicy: &fg,
	})
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("failed to uninstall KUDO manager %s/%s: %v", options.Namespace, kudoinit.DefaultManagerName, err)
		}
	}
	return nil
}

func (m Initializer) verifyManagerNotInstalled(client *kube.Client, result *verifier.Result) error {
	_, err := client.KubeClient.AppsV1().StatefulSets(m.options.Namespace).Get(context.TODO(), kudoinit.DefaultManagerName, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	result.AddErrors(fmt.Sprintf("KUDO manager %s/%s seems to be installed. Did you mean to use --upgrade", m.options.Namespace, kudoinit.DefaultManagerName))
	return nil
}

func (m Initializer) verifyManagerInstalled(client *kube.Client, result *verifier.Result) error {
	mgr, err := client.KubeClient.AppsV1().StatefulSets(m.options.Namespace).Get(context.TODO(), kudoinit.DefaultManagerName, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			result.AddErrors(fmt.Sprintf("failed to find KUDO manager %s/%s", m.options.Namespace, kudoinit.DefaultManagerName))
			return nil
		}
		return err
	}
	if len(mgr.Spec.Template.Spec.Containers) < 1 {
		result.AddErrors("failed to validate KUDO manager. Spec had no containers")
		return nil
	}
	if mgr.Spec.Template.Spec.Containers[0].Image != m.options.Image {
		result.AddWarnings(fmt.Sprintf("KUDO manager has an unexpected image. Expected %s but found %s", m.options.Image, mgr.Spec.Template.Spec.Containers[0].Image))
		return nil
	}
	if err := health.IsHealthy(mgr); err != nil {
		result.AddErrors("KUDO manager seems to be not healthy")
		return nil
	}
	clog.V(2).Printf("KUDO manager is healthy and running %s", mgr.Spec.Template.Spec.Containers[0].Image)
	return nil
}

func (m Initializer) installStatefulSet(client *kube.Client) error {
	clog.V(4).Printf("try to delete manager stateful set %s/%s before creating it", m.deployment.Namespace, m.deployment.Name)
	if err := UninstallStatefulSet(client, m.options); err != nil {
		return err
	}
	_, err := client.KubeClient.AppsV1().StatefulSets(m.options.Namespace).Create(context.TODO(), m.deployment, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to recreate manager stateful set %s/%s: %v", m.options.Namespace, m.deployment.Name, err)
	}
	return nil
}

func (m Initializer) installService(client corev1client.ServicesGetter) error {
	clog.V(4).Printf("try to delete manager service %s/%s before creating it", m.service.Namespace, m.service.Name)
	err := client.Services(m.options.Namespace).Delete(context.TODO(), m.service.Name, metav1.DeleteOptions{})
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete manager service %s/%s for recreation: %v", m.service.Namespace, m.service.Name, err)
		}
	}

	_, err = client.Services(m.options.Namespace).Create(context.TODO(), m.service, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to recreate manager service %s/%s: %v", m.options.Namespace, m.service.Name, err)
	}
	return nil
}

func (m Initializer) Resources() []runtime.Object {
	return []runtime.Object{m.service, m.deployment}
}

// GenerateLabels returns the labels used by deployment and service
func GenerateLabels() labels.Set {
	return kudoinit.GenerateLabels(map[string]string{"control-plane": "controller-manager"})
}

func generateDeployment(opts kudoinit.Options) *appsv1.StatefulSet {
	managerLabels := GenerateLabels()

	secretDefaultMode := int32(420)
	image := opts.Image
	imagePullPolicy := v1.PullPolicy(opts.ImagePullPolicy)
	s := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: opts.Namespace,
			Name:      kudoinit.DefaultManagerName,
			Labels:    managerLabels,
		},
		Spec: appsv1.StatefulSetSpec{
			Selector:    &metav1.LabelSelector{MatchLabels: managerLabels},
			ServiceName: kudoinit.DefaultServiceName,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: managerLabels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: opts.ServiceAccount,
					Containers: []corev1.Container{
						{
							Command: []string{"/root/manager"},
							Env: []corev1.EnvVar{
								{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
								{Name: "SECRET_NAME", Value: kudoinit.DefaultSecretName},
							},
							Image:           image,
							ImagePullPolicy: imagePullPolicy,
							Name:            "manager",
							Ports: []corev1.ContainerPort{
								// name matters for service
								{ContainerPort: 443, Name: "webhook-server", Protocol: "TCP"},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									"cpu":    resource.MustParse("100m"),
									"memory": resource.MustParse("50Mi")},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "cert", MountPath: "/tmp/cert", ReadOnly: true},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  kudoinit.DefaultSecretName,
									DefaultMode: &secretDefaultMode,
								},
							},
						},
					},
					TerminationGracePeriodSeconds: &opts.TerminationGracePeriodSeconds,
				},
			},
		},
	}

	return s
}

func generateService(opts kudoinit.Options) *corev1.Service {
	managerLabels := GenerateLabels()
	s := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: opts.Namespace,
			Name:      kudoinit.DefaultServiceName,
			Labels:    managerLabels,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "kudo",
					Port:       443,
					TargetPort: intstr.FromString("webhook-server")},
			},
			Selector: managerLabels,
		},
	}
	return s
}
