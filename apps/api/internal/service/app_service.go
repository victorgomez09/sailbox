package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/sailboxhq/sailbox/apps/api/internal/model"
	"github.com/sailboxhq/sailbox/apps/api/internal/orchestrator"
	"github.com/sailboxhq/sailbox/apps/api/internal/store"
)

type AppService struct {
	store     store.Store
	orch      orchestrator.Orchestrator
	logger    *slog.Logger
	domainSvc *DomainService
}

func NewAppService(s store.Store, orch orchestrator.Orchestrator, logger *slog.Logger, domainSvc *DomainService) *AppService {
	return &AppService{store: s, orch: orch, logger: logger, domainSvc: domainSvc}
}

type CreateAppInput struct {
	ProjectID     uuid.UUID           `json:"project_id" binding:"required"`
	Name          string              `json:"name" binding:"required,min=1,max=63"`
	SourceType    model.SourceType    `json:"source_type" binding:"required,oneof=git image compose"`
	GitRepo       string              `json:"git_repo" binding:"required_if=SourceType git"`
	GitBranch     string              `json:"git_branch"`
	GitProviderID *uuid.UUID          `json:"git_provider_id"`
	DockerImage   string              `json:"docker_image" binding:"required_if=SourceType image"`
	BuildType     model.BuildType     `json:"build_type"`
	Dockerfile    string              `json:"dockerfile"`
	Replicas      int32               `json:"replicas"`
	CPULimit      string              `json:"cpu_limit"`
	MemLimit      string              `json:"mem_limit"`
	EnvVars       map[string]string   `json:"env_vars"`
	Ports         []model.PortMapping `json:"ports"`
}

func (s *AppService) Create(ctx context.Context, input CreateAppInput) (*model.Application, error) {
	app := &model.Application{
		ProjectID:     input.ProjectID,
		Name:          input.Name,
		SourceType:    input.SourceType,
		GitRepo:       input.GitRepo,
		GitBranch:     input.GitBranch,
		GitProviderID: input.GitProviderID,
		DockerImage:   input.DockerImage,
		BuildType:     input.BuildType,
		Dockerfile:    input.Dockerfile,
		Replicas:      input.Replicas,
		CPULimit:      input.CPULimit,
		MemLimit:      input.MemLimit,
		EnvVars:       input.EnvVars,
		Ports:         input.Ports,
		Status:        model.AppStatusIdle,
	}

	// Apply defaults
	if app.GitBranch == "" {
		app.GitBranch = "main"
	}
	if app.Replicas == 0 {
		app.Replicas = 1
	}
	if app.CPULimit == "" {
		app.CPULimit = "500m"
	}
	if app.MemLimit == "" {
		app.MemLimit = "512Mi"
	}
	if app.BuildType == "" && app.SourceType == model.SourceGit {
		app.BuildType = model.BuildDockerfile
	}
	if app.Dockerfile == "" {
		app.Dockerfile = "Dockerfile"
	}

	// Default container port based on common images
	if len(app.Ports) == 0 {
		port := guessContainerPort(app.DockerImage)
		app.Ports = []model.PortMapping{{ContainerPort: port, ServicePort: port, Protocol: "tcp"}}
	}

	// Look up project namespace — app requires a valid namespace
	project, err := s.store.Projects().GetByID(ctx, input.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}
	if project.Namespace == "" {
		return nil, fmt.Errorf("project has no namespace configured")
	}
	app.Namespace = project.Namespace

	// Validate git provider belongs to same org
	if app.GitProviderID != nil {
		res, err := s.store.SharedResources().GetByID(ctx, *app.GitProviderID)
		if err != nil {
			return nil, fmt.Errorf("git provider not found: %w", err)
		}
		if res.OrgID != project.OrgID {
			return nil, fmt.Errorf("git provider does not belong to this organization")
		}
	}

	if err := s.store.Applications().Create(ctx, app); err != nil {
		return nil, err
	}

	// Auto-generate default Traefik domain
	if s.domainSvc != nil {
		if domain, err := s.domainSvc.GenerateTraefikDomain(ctx, app.ID); err != nil {
			s.logger.Error("failed to auto-generate domain", slog.Any("error", err))
		} else {
			s.logger.Info("default domain generated", slog.String("host", domain.Host))
		}
	}

	s.logger.Info("application created",
		slog.String("name", app.Name),
		slog.String("id", app.ID.String()),
		slog.String("source", string(app.SourceType)),
	)
	return app, nil
}

// sanitizeName converts a name to a valid DNS subdomain label.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.ReplaceAll(name, " ", "-")
	return name
}

// guessContainerPort returns a default port for common Docker images.
func guessContainerPort(image string) int {
	img := strings.ToLower(image)
	switch {
	case strings.Contains(img, "nginx"), strings.Contains(img, "httpd"), strings.Contains(img, "apache"):
		return 80
	case strings.Contains(img, "node"), strings.Contains(img, "next"), strings.Contains(img, "nuxt"):
		return 3000
	case strings.Contains(img, "rails"), strings.Contains(img, "puma"):
		return 3000
	case strings.Contains(img, "django"), strings.Contains(img, "flask"), strings.Contains(img, "uvicorn"):
		return 8000
	case strings.Contains(img, "spring"), strings.Contains(img, "tomcat"):
		return 8080
	case strings.Contains(img, "go"), strings.Contains(img, "gin"), strings.Contains(img, "fiber"):
		return 8080
	case strings.Contains(img, "postgres"):
		return 5432
	case strings.Contains(img, "mysql"), strings.Contains(img, "mariadb"):
		return 3306
	case strings.Contains(img, "redis"), strings.Contains(img, "valkey"):
		return 6379
	case strings.Contains(img, "mongo"):
		return 27017
	default:
		return 80
	}
}

type UpdateAppInput struct {
	// Source configuration
	GitRepo     *string `json:"git_repo"`
	GitBranch   *string `json:"git_branch"`
	DockerImage *string `json:"docker_image"`

	// Build configuration
	BuildType    *string           `json:"build_type"`
	Dockerfile   *string           `json:"dockerfile"`
	BuildContext *string           `json:"build_context"`
	BuildArgs    map[string]string `json:"build_args"`
	BuildEnvVars map[string]string `json:"build_env_vars"`
	WatchPaths   []string          `json:"watch_paths"`
	NoCache      *bool             `json:"no_cache"`

	// Runtime configuration
	CPULimit   *string             `json:"cpu_limit"`
	MemLimit   *string             `json:"mem_limit"`
	CPURequest *string             `json:"cpu_request"`
	MemRequest *string             `json:"mem_request"`
	Ports      []model.PortMapping `json:"ports"`
	NodePool   *string             `json:"node_pool"`

	// Advanced configuration
	HealthCheck            *model.HealthCheck          `json:"health_check"`
	Autoscaling            *model.AutoscalingConfig    `json:"autoscaling"`
	Volumes                []model.VolumeMount         `json:"volumes"`
	DeployStrategy         *string                     `json:"deploy_strategy"`
	DeployStrategyConfig   *model.DeployStrategyConfig `json:"deploy_strategy_config"`
	TerminationGracePeriod *int                        `json:"termination_grace_period"`
}

func (s *AppService) Update(ctx context.Context, id uuid.UUID, input UpdateAppInput) (*model.Application, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Track whether runtime-affecting fields changed (need K8s deployment update)
	runtimeChanged := false

	// Apply source fields
	if input.GitRepo != nil {
		app.GitRepo = *input.GitRepo
	}
	if input.GitBranch != nil {
		app.GitBranch = *input.GitBranch
	}
	if input.DockerImage != nil {
		app.DockerImage = *input.DockerImage
	}

	// Apply build fields
	if input.BuildType != nil {
		app.BuildType = model.BuildType(*input.BuildType)
	}
	if input.Dockerfile != nil {
		app.Dockerfile = *input.Dockerfile
	}
	if input.BuildContext != nil {
		app.BuildContext = *input.BuildContext
	}
	if input.BuildArgs != nil {
		app.BuildArgs = input.BuildArgs
	}
	if input.BuildEnvVars != nil {
		app.BuildEnvVars = input.BuildEnvVars
	}
	if input.WatchPaths != nil {
		app.WatchPaths = input.WatchPaths
	}
	if input.NoCache != nil {
		app.NoCache = *input.NoCache
	}

	// Apply runtime fields (these affect the live K8s deployment)
	if input.CPULimit != nil {
		app.CPULimit = *input.CPULimit
		runtimeChanged = true
	}
	if input.MemLimit != nil {
		app.MemLimit = *input.MemLimit
		runtimeChanged = true
	}
	if input.CPURequest != nil {
		app.CPURequest = *input.CPURequest
		runtimeChanged = true
	}
	if input.MemRequest != nil {
		app.MemRequest = *input.MemRequest
		runtimeChanged = true
	}
	if input.Ports != nil {
		app.Ports = input.Ports
		runtimeChanged = true
	}
	if input.NodePool != nil {
		app.NodePool = *input.NodePool
		runtimeChanged = true
	}
	if input.HealthCheck != nil {
		app.HealthCheck = input.HealthCheck
		runtimeChanged = true
	}
	if input.Autoscaling != nil {
		app.Autoscaling = input.Autoscaling
	}
	if input.Volumes != nil {
		app.Volumes = input.Volumes
		runtimeChanged = true
	}
	if input.DeployStrategy != nil {
		app.DeployStrategy = *input.DeployStrategy
		runtimeChanged = true
	}
	if input.DeployStrategyConfig != nil {
		app.DeployStrategyConfig = input.DeployStrategyConfig
		runtimeChanged = true
	}
	if input.TerminationGracePeriod != nil {
		app.TerminationGracePeriod = *input.TerminationGracePeriod
		runtimeChanged = true
	}

	if err := s.store.Applications().Update(ctx, app); err != nil {
		return nil, err
	}

	// Reconcile K8s resources if autoscaling changed
	if input.Autoscaling != nil {
		if input.Autoscaling.Enabled {
			if err := s.orch.ConfigureHPA(ctx, app, *input.Autoscaling); err != nil {
				s.logger.Error("failed to configure HPA", slog.Any("error", err))
			}
		} else {
			if err := s.orch.DeleteHPA(ctx, app); err != nil {
				s.logger.Error("failed to delete HPA", slog.Any("error", err))
			}
		}
	}

	// Redeploy if runtime-affecting fields changed and app is currently deployed
	if runtimeChanged && app.Status == model.AppStatusRunning {
		mergedEnv := make(map[string]string)
		project, projErr := s.store.Projects().GetByID(ctx, app.ProjectID)
		if projErr == nil && project.EnvVars != nil {
			for k, v := range project.EnvVars {
				mergedEnv[k] = v
			}
		}
		for k, v := range app.EnvVars {
			mergedEnv[k] = v
		}
		if err := s.orch.Deploy(ctx, app, orchestrator.DeployOpts{
			Image:                  app.DockerImage,
			Replicas:               app.Replicas,
			EnvVars:                mergedEnv,
			Ports:                  app.Ports,
			CPULimit:               app.CPULimit,
			MemLimit:               app.MemLimit,
			CPURequest:             app.CPURequest,
			MemRequest:             app.MemRequest,
			HealthCheck:            app.HealthCheck,
			Volumes:                app.Volumes,
			DeployStrategy:         app.DeployStrategy,
			DeployStrategyConfig:   app.DeployStrategyConfig,
			TerminationGracePeriod: app.TerminationGracePeriod,
			NodePool:               app.NodePool,
		}); err != nil {
			s.logger.Warn("failed to apply runtime changes to deployment — will take effect on next deploy",
				slog.String("app", app.Name), slog.Any("error", err))
		}
	}

	s.logger.Info("application updated", slog.String("name", app.Name), slog.String("id", app.ID.String()))
	return app, nil
}

func (s *AppService) GetPodEvents(ctx context.Context, id uuid.UUID, podName string) ([]orchestrator.PodEvent, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.orch.GetPodEvents(ctx, app, podName)
}

func (s *AppService) GetByID(ctx context.Context, id uuid.UUID) (*model.Application, error) {
	return s.store.Applications().GetByID(ctx, id)
}

func (s *AppService) List(ctx context.Context, projectID uuid.UUID, params store.ListParams) ([]model.Application, int, error) {
	return s.store.Applications().ListByProject(ctx, projectID, params)
}

func (s *AppService) ListAll(ctx context.Context, params store.ListParams, filter store.AppListFilter) ([]model.Application, int, error) {
	return s.store.Applications().ListAll(ctx, params, filter)
}

func (s *AppService) Delete(ctx context.Context, id uuid.UUID) error {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return err
	}

	// Delete K8s resources
	if err := s.orch.Delete(ctx, app); err != nil {
		s.logger.Error("failed to delete app from orchestrator", slog.Any("error", err), slog.String("app", app.Name))
	}

	// Delete associated Ingress resources
	domains, _ := s.store.Domains().ListByApp(ctx, id)
	for _, d := range domains {
		if err := s.orch.DeleteIngress(ctx, &d); err != nil {
			s.logger.Warn("failed to delete ingress", slog.String("domain", d.Host), slog.Any("error", err))
		}
	}

	return s.store.Applications().Delete(ctx, id)
}

func (s *AppService) Scale(ctx context.Context, id uuid.UUID, replicas int32) (*model.Application, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := s.orch.Scale(ctx, app, replicas); err != nil {
		return nil, err
	}

	app.Replicas = replicas
	if err := s.store.Applications().Update(ctx, app); err != nil {
		return nil, err
	}
	return app, nil
}

func (s *AppService) UpdateEnvVars(ctx context.Context, id uuid.UUID, envVars map[string]string) (*model.Application, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	app.EnvVars = envVars
	if err := s.store.Applications().Update(ctx, app); err != nil {
		return nil, err
	}

	// Merge project env + app env and push to running deployment
	mergedEnv := make(map[string]string)
	project, projErr := s.store.Projects().GetByID(ctx, app.ProjectID)
	if projErr == nil && project.EnvVars != nil {
		for k, v := range project.EnvVars {
			mergedEnv[k] = v
		}
	}
	for k, v := range app.EnvVars {
		mergedEnv[k] = v
	}
	if err := s.orch.UpdateEnvVars(ctx, app, mergedEnv); err != nil {
		s.logger.Warn("failed to push env vars to deployment — will take effect on next deploy",
			slog.String("app", app.Name), slog.Any("error", err))
	}

	s.logger.Info("env vars updated", slog.String("app", app.Name), slog.Int("count", len(envVars)))
	return app, nil
}

func (s *AppService) GetStatus(ctx context.Context, id uuid.UUID) (*orchestrator.AppStatus, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	status, err := s.orch.GetStatus(ctx, app)
	if err != nil {
		return nil, err
	}

	// Reconcile DB status with live K8s status.
	// Only update DB when K8s has settled into a stable state AND
	// the DB still holds a transitional status from a previous operation.
	// Reconcile DB status with actual K8s state
	var dbStatus model.AppStatus
	switch status.Phase {
	case "running":
		dbStatus = model.AppStatusRunning
	case "stopped":
		dbStatus = model.AppStatusStopped
	case "failed":
		dbStatus = model.AppStatusError
	case "not deployed":
		dbStatus = model.AppStatusStopped
	}
	// Update DB if K8s state differs from stored status
	if dbStatus != "" && dbStatus != app.Status {
		_ = s.store.Applications().UpdateStatus(ctx, id, dbStatus)
	}

	return status, nil
}

func (s *AppService) GetPods(ctx context.Context, id uuid.UUID) ([]orchestrator.PodInfo, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.orch.GetPods(ctx, app)
}

func (s *AppService) Restart(ctx context.Context, id uuid.UUID) error {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return err
	}

	// Mark as restarting — K8s rolling restart takes time.
	// Status will be reconciled by the orchestrator's GetStatus on next query.
	_ = s.store.Applications().UpdateStatus(ctx, id, model.AppStatusRestarting)

	if err := s.orch.Restart(ctx, app); err != nil {
		_ = s.store.Applications().UpdateStatus(ctx, id, model.AppStatusError)
		return err
	}

	// Don't set back to "running" here — let the UI query GetStatus from K8s
	// to get the real state (pods may still be rolling).
	return nil
}

func (s *AppService) Stop(ctx context.Context, id uuid.UUID) error {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return err
	}

	_ = s.store.Applications().UpdateStatus(ctx, id, model.AppStatusStopping)

	if err := s.orch.Stop(ctx, app); err != nil {
		_ = s.store.Applications().UpdateStatus(ctx, id, model.AppStatusError)
		return err
	}
	return s.store.Applications().UpdateStatus(ctx, id, model.AppStatusStopped)
}

func (s *AppService) ClearBuildCache(ctx context.Context, id uuid.UUID) error {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return err
	}
	return s.orch.ClearBuildCache(ctx, app)
}

// ============================================================================
// Webhook Management
// ============================================================================

type WebhookConfig struct {
	WebhookURL string `json:"webhook_url"`
	Secret     string `json:"secret"`
	AutoDeploy bool   `json:"auto_deploy"`
}

func (s *AppService) EnableWebhook(ctx context.Context, id uuid.UUID, baseURL string) (*WebhookConfig, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Generate webhook secret
	secret := "whsec_" + randomHex(16)
	app.WebhookSecret = secret
	app.AutoDeploy = true

	if err := s.store.Applications().Update(ctx, app); err != nil {
		return nil, err
	}

	// Determine webhook URL based on source type/provider
	provider := "github"
	if strings.Contains(app.GitRepo, "gitlab") {
		provider = "gitlab"
	}

	webhookURL := fmt.Sprintf("%s/api/v1/webhooks/%s/%s", baseURL, provider, app.ID)

	return &WebhookConfig{
		WebhookURL: webhookURL,
		Secret:     secret,
		AutoDeploy: true,
	}, nil
}

func (s *AppService) DisableWebhook(ctx context.Context, id uuid.UUID) error {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return err
	}
	app.AutoDeploy = false
	return s.store.Applications().Update(ctx, app)
}

func (s *AppService) RegenerateWebhookSecret(ctx context.Context, id uuid.UUID, baseURL string) (*WebhookConfig, error) {
	return s.EnableWebhook(ctx, id, baseURL)
}

func (s *AppService) GetWebhookConfig(ctx context.Context, id uuid.UUID, baseURL string) (*WebhookConfig, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	provider := "github"
	if strings.Contains(app.GitRepo, "gitlab") {
		provider = "gitlab"
	}

	return &WebhookConfig{
		WebhookURL: fmt.Sprintf("%s/api/v1/webhooks/%s/%s", baseURL, provider, app.ID),
		Secret:     app.WebhookSecret,
		AutoDeploy: app.AutoDeploy,
	}, nil
}

// ============================================================================
// Secrets Management
// ============================================================================

func (s *AppService) GetSecretKeys(ctx context.Context, id uuid.UUID) ([]string, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(app.Secrets))
	for k := range app.Secrets {
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *AppService) UpdateSecrets(ctx context.Context, id uuid.UUID, secrets map[string]string) ([]string, error) {
	app, err := s.store.Applications().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	app.Secrets = secrets
	if err := s.store.Applications().Update(ctx, app); err != nil {
		return nil, err
	}

	// Create/update the K8s Secret
	if err := s.orch.EnsureSecret(ctx, app, secrets); err != nil {
		s.logger.Error("failed to ensure K8s secret", slog.Any("error", err), slog.String("app", app.Name))
	}

	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	s.logger.Info("secrets updated", slog.String("app", app.Name), slog.Int("count", len(secrets)))
	return keys, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
