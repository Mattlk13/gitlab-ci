package kubernetes

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/context"
	"k8s.io/kubernetes/pkg/api"
	client "k8s.io/kubernetes/pkg/client/unversioned"

	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/common"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/executors"
)

var (
	executorOptions = executors.ExecutorOptions{
		SharedBuildsDir: false,
		Shell: common.ShellScriptInfo{
			Shell:         "bash",
			Type:          common.NormalShell,
			RunnerCommand: "/usr/bin/gitlab-runner-helper",
		},
		ShowHostname:     true,
		SupportedOptions: []string{"image", "services", "artifacts", "cache"},
	}
)

type kubernetesOptions struct {
	Image    string   `json:"image"`
	Services []string `json:"services"`
}

type executor struct {
	executors.AbstractExecutor

	kubeClient *client.Client
	pod        *api.Pod
	options    *kubernetesOptions

	namespaceOverwrite string

	buildLimits     api.ResourceList
	serviceLimits   api.ResourceList
	helperLimits    api.ResourceList
	buildRequests   api.ResourceList
	serviceRequests api.ResourceList
	helperRequests  api.ResourceList
	pullPolicy      common.KubernetesPullPolicy
}

func (s *executor) setupResources() error {
	var err error

	// Limit
	CPULimit := getNewOrLegacy(s.Config.Kubernetes.CPULimit, s.Config.Kubernetes.CPUs)
	MemoryLimit := getNewOrLegacy(s.Config.Kubernetes.MemoryLimit, s.Config.Kubernetes.Memory)

	if s.buildLimits, err = limits(CPULimit, MemoryLimit); err != nil {
		return fmt.Errorf("invalid build limits specified: %s", err.Error())
	}

	CPULimit = getNewOrLegacy(s.Config.Kubernetes.ServiceCPULimit, s.Config.Kubernetes.ServiceCPUs)
	MemoryLimit = getNewOrLegacy(s.Config.Kubernetes.ServiceMemoryLimit, s.Config.Kubernetes.ServiceMemory)

	if s.serviceLimits, err = limits(CPULimit, MemoryLimit); err != nil {
		return fmt.Errorf("invalid service limits specified: %s", err.Error())
	}

	CPULimit = getNewOrLegacy(s.Config.Kubernetes.HelperCPULimit, s.Config.Kubernetes.HelperCPUs)
	MemoryLimit = getNewOrLegacy(s.Config.Kubernetes.HelperMemoryLimit, s.Config.Kubernetes.HelperMemory)

	if s.helperLimits, err = limits(CPULimit, MemoryLimit); err != nil {
		return fmt.Errorf("invalid helper limits specified: %s", err.Error())
	}

	// Requests
	if s.buildRequests, err = limits(s.Config.Kubernetes.CPURequest, s.Config.Kubernetes.MemoryRequest); err != nil {
		return fmt.Errorf("invalid build requests specified: %s", err.Error())
	}

	if s.serviceRequests, err = limits(s.Config.Kubernetes.ServiceCPURequest, s.Config.Kubernetes.ServiceMemoryRequest); err != nil {
		return fmt.Errorf("invalid service requests specified: %s", err.Error())
	}

	if s.helperRequests, err = limits(s.Config.Kubernetes.HelperCPURequest, s.Config.Kubernetes.HelperMemoryRequest); err != nil {
		return fmt.Errorf("invalid helper requests specified: %s", err.Error())
	}

	return nil
}

func (s *executor) Prepare(globalConfig *common.Config, config *common.RunnerConfig, build *common.Build) (err error) {
	if err = s.AbstractExecutor.Prepare(globalConfig, config, build); err != nil {
		return err
	}

	if s.BuildShell.PassFile {
		return fmt.Errorf("kubernetes doesn't support shells that require script file")
	}

	if err = build.Options.Decode(&s.options); err != nil {
		return err
	}

	if s.kubeClient, err = getKubeClient(config.Kubernetes); err != nil {
		return fmt.Errorf("error connecting to Kubernetes: %s", err.Error())
	}

	if err = s.setupResources(); err != nil {
		return err
	}

	if s.pullPolicy, err = s.Config.Kubernetes.PullPolicy.Get(); err != nil {
		return err
	}

	if err = s.overwriteNamespace(build); err != nil {
		return err
	}

	if err = s.checkDefaults(); err != nil {
		return err
	}

	s.Println("Using Kubernetes executor with image", s.options.Image, "...")

	return nil
}

func (s *executor) Run(cmd common.ExecutorCommand) error {
	s.Debugln("Starting Kubernetes command...")

	if s.pod == nil {
		err := s.setupBuildPod()

		if err != nil {
			return err
		}
	}

	containerName := "build"
	if cmd.Predefined {
		containerName = "helper"
	}

	ctx, cancel := context.WithCancel(context.Background())
	select {
	case err := <-s.runInContainer(ctx, containerName, cmd.Script):
		if err != nil && strings.Contains(err.Error(), "executing in Docker Container") {
			return &common.BuildError{Inner: err}
		}
		return err
	case <-cmd.Abort:
		cancel()
		return fmt.Errorf("build aborted")
	}
}

func (s *executor) Cleanup() {
	if s.pod != nil {
		err := s.kubeClient.Pods(s.pod.Namespace).Delete(s.pod.Name, nil)
		if err != nil {
			s.Errorln(fmt.Sprintf("Error cleaning up pod: %s", err.Error()))
		}
	}
	closeKubeClient(s.kubeClient)
	s.AbstractExecutor.Cleanup()
}

func (s *executor) buildContainer(name, image string, requests, limits api.ResourceList, command ...string) api.Container {
	path := strings.Split(s.Build.BuildDir, "/")
	path = path[:len(path)-1]

	privileged := false
	if s.Config.Kubernetes != nil {
		privileged = s.Config.Kubernetes.Privileged
	}

	return api.Container{
		Name:            name,
		Image:           image,
		ImagePullPolicy: api.PullPolicy(s.pullPolicy),
		Command:         command,
		Env:             buildVariables(s.Build.GetAllVariables().PublicOrInternal()),
		Resources: api.ResourceRequirements{
			Limits:   limits,
			Requests: requests,
		},
		VolumeMounts: []api.VolumeMount{
			api.VolumeMount{
				Name:      "repo",
				MountPath: strings.Join(path, "/"),
			},
		},
		SecurityContext: &api.SecurityContext{
			Privileged: &privileged,
		},
		Stdin: true,
	}
}

func (s *executor) setupBuildPod() error {
	services := make([]api.Container, len(s.options.Services))
	for i, image := range s.options.Services {
		resolvedImage := s.Build.GetAllVariables().ExpandValue(image)
		services[i] = s.buildContainer(fmt.Sprintf("svc-%d", i), resolvedImage, s.serviceRequests, s.serviceLimits)
	}

	var imagePullSecrets []api.LocalObjectReference
	for _, imagePullSecret := range s.Config.Kubernetes.ImagePullSecrets {
		imagePullSecrets = append(imagePullSecrets, api.LocalObjectReference{Name: imagePullSecret})
	}

	buildImage := s.Build.GetAllVariables().ExpandValue(s.options.Image)

	pod, err := s.kubeClient.Pods(s.Config.Kubernetes.Namespace).Create(&api.Pod{
		ObjectMeta: api.ObjectMeta{
			GenerateName: s.Build.ProjectUniqueName(),
			Namespace:    s.Config.Kubernetes.Namespace,
		},
		Spec: api.PodSpec{
			Volumes: []api.Volume{
				api.Volume{
					Name: "repo",
					VolumeSource: api.VolumeSource{
						EmptyDir: &api.EmptyDirVolumeSource{},
					},
				},
			},
			RestartPolicy: api.RestartPolicyNever,
			NodeSelector:  s.Config.Kubernetes.NodeSelector,
			Containers: append([]api.Container{
				s.buildContainer("build", buildImage, s.buildRequests, s.buildLimits, s.BuildShell.DockerCommand...),
				s.buildContainer("helper", s.Config.Kubernetes.GetHelperImage(), s.helperRequests, s.helperLimits, s.BuildShell.DockerCommand...),
			}, services...),
			TerminationGracePeriodSeconds: &s.Config.Kubernetes.TerminationGracePeriodSeconds,
			ImagePullSecrets:              imagePullSecrets,
		},
	})
	if err != nil {
		return err
	}

	s.pod = pod

	return nil
}

func (s *executor) runInContainer(ctx context.Context, name, command string) <-chan error {
	errc := make(chan error, 1)
	go func() {
		defer close(errc)

		status, err := waitForPodRunning(ctx, s.kubeClient, s.pod, s.BuildTrace, s.Config.Kubernetes)

		if err != nil {
			errc <- err
			return
		}

		if status != api.PodRunning {
			errc <- fmt.Errorf("pod failed to enter running state: %s", status)
			return
		}

		config, err := getKubeClientConfig(s.Config.Kubernetes)

		if err != nil {
			errc <- err
			return
		}

		exec := ExecOptions{
			PodName:       s.pod.Name,
			Namespace:     s.pod.Namespace,
			ContainerName: name,
			Command:       s.BuildShell.DockerCommand,
			In:            strings.NewReader(command),
			Out:           s.BuildTrace,
			Err:           s.BuildTrace,
			Stdin:         true,
			Config:        config,
			Client:        s.kubeClient,
			Executor:      &DefaultRemoteExecutor{},
		}

		errc <- exec.Run()
	}()

	return errc
}

// checkDefaults Defines the configuration for the Pod on Kubernetes
func (s *executor) checkDefaults() error {
	if s.options.Image == "" {
		if s.Config.Kubernetes.Image == "" {
			return fmt.Errorf("no image specified and no default set in config")
		}

		s.options.Image = s.Config.Kubernetes.Image
	}

	if s.Config.Kubernetes.Namespace == "" {
		s.Warningln("Namespace is empty, therefore assuming 'default'.")
		s.Config.Kubernetes.Namespace = "default"
	}

	s.Println("Using Kubernetes namespace:", s.Config.Kubernetes.Namespace)

	return nil
}

// overwriteNamespace checks for variable in order to overwrite the configured
// namespace, as long as it complies to validation regular-expression, when
// expression is empty the overwrite is disabled.
func (s *executor) overwriteNamespace(build *common.Build) error {
	if s.Config.Kubernetes.NamespaceOverwriteAllowed == "" {
		s.Debugln("Configuration entry 'namespace_overwrite_allowed' is empty, using configured namespace.")
		return nil
	}

	// looking for namespace overwrite variable, and expanding for interpolation
	s.namespaceOverwrite = build.Variables.Expand().Get("KUBERNETES_NAMESPACE_OVERWRITE")
	if s.namespaceOverwrite == "" {
		return nil
	}

	var err error
	var r *regexp.Regexp
	if r, err = regexp.Compile(s.Config.Kubernetes.NamespaceOverwriteAllowed); err != nil {
		return err
	}

	if match := r.MatchString(s.namespaceOverwrite); !match {
		return fmt.Errorf("KUBERNETES_NAMESPACE_OVERWRITE='%s' does not match 'namespace_overwrite_allowed': '%s'",
			s.namespaceOverwrite, s.Config.Kubernetes.NamespaceOverwriteAllowed)
	}

	s.Println("Overwritting configured namespace, from", s.Config.Kubernetes.Namespace, "to", s.namespaceOverwrite)
	s.Config.Kubernetes.Namespace = s.namespaceOverwrite

	return nil
}

func createFn() common.Executor {
	return &executor{
		AbstractExecutor: executors.AbstractExecutor{
			ExecutorOptions: executorOptions,
		},
	}
}

func featuresFn(features *common.FeaturesInfo) {
	features.Variables = true
	features.Image = true
	features.Services = true
	features.Artifacts = true
	features.Cache = true
}

func init() {
	common.RegisterExecutor("kubernetes", executors.DefaultExecutorProvider{
		Creator:         createFn,
		FeaturesUpdater: featuresFn,
	})
}
