package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/sailboxhq/sailbox/apps/api/internal/model"
	"github.com/sailboxhq/sailbox/apps/api/internal/orchestrator"
	"github.com/sailboxhq/sailbox/apps/api/internal/store"
)

type DomainService struct {
	store      store.Store
	orch       orchestrator.RouteManager // Interfaz refactorizada (Gateway API)
	logger     *slog.Logger
	settingSvc *SettingService
}

func NewDomainService(s store.Store, orch orchestrator.RouteManager, logger *slog.Logger, settingSvc *SettingService) *DomainService {
	return &DomainService{
		store:      s,
		orch:       orch,
		logger:     logger,
		settingSvc: settingSvc,
	}
}

type CreateDomainInput struct {
	Host     string `json:"host" binding:"required"`
	TLS      bool   `json:"tls"`
	AutoCert bool   `json:"auto_cert"`
}

// --- Helpers de Validación y DNS ---

func normalizeDomainHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.ToLower(host)
	host = strings.TrimRight(host, ".")
	return host
}

func validateDomainHost(host string) error {
	if host == "" {
		return errors.New("hostname cannot be empty")
	}
	if len(host) > 253 {
		return errors.New("hostname must be 253 characters or fewer")
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return fmt.Errorf("each label must be 1-63 characters, got %q", label)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("label %q must not start or end with a hyphen", label)
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return fmt.Errorf("invalid character %q in hostname", c)
			}
		}
	}
	if len(labels) < 2 && host != "localhost" {
		return errors.New("hostname must have at least two labels (e.g. app.example.com)")
	}
	return nil
}

func isDevelopmentDomain(base string) bool {
	devSuffixes := []string{".localhost", ".local", ".test", "nip.io", "sslip.io", "traefik.me"}
	for _, suffix := range devSuffixes {
		if strings.Contains(base, suffix) {
			return true
		}
	}
	return base == "localhost"
}

// --- Métodos del Servicio ---

func (s *DomainService) Create(ctx context.Context, appID uuid.UUID, input CreateDomainInput) (*model.Domain, error) {
	input.Host = normalizeDomainHost(input.Host)
	if err := validateDomainHost(input.Host); err != nil {
		return nil, err
	}

	app, err := s.store.Applications().GetByID(ctx, appID)
	if err != nil {
		return nil, err
	}

	domain := &model.Domain{
		AppID:    appID,
		Host:     input.Host,
		TLS:      input.TLS,
		AutoCert: input.AutoCert,
	}

	// 1. Persistencia en DB (Check de duplicados)
	if err := s.store.Domains().Create(ctx, domain); err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			return nil, errors.New("domain already in use")
		}
		return nil, err
	}

	// 2. Creación de HTTPRoute en K8s via Gateway API
	if err := s.orch.CreateRoute(ctx, domain, app); err != nil {
		_ = s.store.Domains().Delete(ctx, domain.ID) // Rollback
		return nil, fmt.Errorf("gateway route creation failed: %w", err)
	}

	// 3. Sync de estado inicial
	if status, sErr := s.orch.GetRouteStatus(ctx, domain, app); sErr == nil {
		domain.IngressReady = status.Ready
		_ = s.store.Domains().Update(ctx, domain)
	}

	s.logger.Info("domain created", slog.String("host", domain.Host), slog.String("app", app.Name))
	return domain, nil
}

func (s *DomainService) GenerateAutoDomain(ctx context.Context, appID uuid.UUID) (*model.Domain, error) {
	baseDomain := s.settingSvc.GetBaseDomain(ctx)
	if baseDomain == "" {
		return nil, errors.New("base domain not configured — go to Settings to set it up")
	}

	app, err := s.store.Applications().GetByID(ctx, appID)
	if err != nil {
		return nil, err
	}

	// Evitar duplicados si ya existe un subdominio generado
	existing, _ := s.store.Domains().ListByApp(ctx, appID)
	for _, d := range existing {
		if strings.HasSuffix(d.Host, "."+baseDomain) {
			_ = s.orch.CreateRoute(ctx, &d, app) // Asegurar que la ruta existe en K8s
			return &d, nil
		}
	}

	// Generar hostname: <app-name>-<random>.<base>
	name := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, strings.ToLower(app.Name))
	name = strings.Trim(name, "-")
	if name == "" {
		name = "app"
	}
	if len(name) > 40 {
		name = name[:40]
	}

	host := fmt.Sprintf("%s-%s.%s", name, randomShort(4), baseDomain)
	useTLS := !isDevelopmentDomain(baseDomain)

	domain := &model.Domain{
		AppID:    appID,
		Host:     host,
		TLS:      useTLS,
		AutoCert: useTLS,
	}

	if err := s.store.Domains().Create(ctx, domain); err != nil {
		return nil, err
	}

	if err := s.orch.CreateRoute(ctx, domain, app); err != nil {
		_ = s.store.Domains().Delete(ctx, domain.ID)
		return nil, fmt.Errorf("failed to create auto-route: %w", err)
	}

	// Sincronizar status final
	if status, sErr := s.orch.GetRouteStatus(ctx, domain, app); sErr == nil {
		domain.IngressReady = status.Ready
	}
	_ = s.store.Domains().Update(ctx, domain)

	return domain, nil
}

func (s *DomainService) Update(ctx context.Context, id uuid.UUID, host *string, forceHTTPS *bool) (*model.Domain, error) {
	domain, err := s.store.Domains().GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	app, err := s.store.Applications().GetByID(ctx, domain.AppID)
	if err != nil {
		return nil, err
	}

	oldHost := domain.Host
	oldForceHTTPS := domain.ForceHTTPS
	hostChanged := false

	if host != nil && *host != "" && *host != domain.Host {
		normalized := normalizeDomainHost(*host)
		if err := validateDomainHost(normalized); err != nil {
			return nil, err
		}

		if domain.TLS && !domain.AutoCert {
			return nil, errors.New("cannot rename domain with manual TLS certificates")
		}
		domain.Host = normalized
		hostChanged = true
	}

	if forceHTTPS != nil {
		domain.ForceHTTPS = *forceHTTPS
	}

	if hostChanged {
		// 1. Actualizar DB primero (uniqueness check)
		if err := s.store.Domains().Update(ctx, domain); err != nil {
			return nil, err
		}

		// 2. Crear nueva ruta en K8s (Zero Downtime)
		if err := s.orch.CreateRoute(ctx, domain, app); err != nil {
			domain.Host = oldHost
			domain.ForceHTTPS = oldForceHTTPS
			_ = s.store.Domains().Update(ctx, domain) // Rollback DB
			return nil, fmt.Errorf("migration to new route failed: %w", err)
		}

		// 3. Borrar ruta antigua
		oldName := s.orch.RouteName(app, oldHost)
		_ = s.orch.DeleteRouteByName(ctx, app, oldName)

	} else {
		// Solo cambio de parámetros (ForceHTTPS)
		if err := s.store.Domains().Update(ctx, domain); err != nil {
			return nil, err
		}
		if err := s.orch.UpdateRoute(ctx, domain, app); err != nil {
			s.logger.Error("failed to update route params", slog.Any("error", err))
		}
	}

	return domain, nil
}

func (s *DomainService) ListByApp(ctx context.Context, appID uuid.UUID) ([]model.Domain, error) {
	domains, err := s.store.Domains().ListByApp(ctx, appID)
	if err != nil {
		return nil, err
	}

	app, appErr := s.store.Applications().GetByID(ctx, appID)
	if appErr != nil {
		return domains, nil
	}

	for i := range domains {
		changed := false

		// Sincronizar Ready Status de Gateway API
		status, sErr := s.orch.GetRouteStatus(ctx, &domains[i], app)
		if sErr == nil && status.Ready != domains[i].IngressReady {
			domains[i].IngressReady = status.Ready
			changed = true
		}

		// Cleanup de marcas legacy de certificados
		if domains[i].TLS && domains[i].CertSecret != "managed-by-gateway" {
			domains[i].CertSecret = "managed-by-gateway"
			changed = true
		}

		// Actualizar expiración si aplica
		if domains[i].TLS {
			expiry, cErr := s.orch.GetCertExpiry(ctx, &domains[i], app)
			if cErr == nil && expiry != nil {
				if domains[i].CertExpiry == nil || !expiry.Equal(*domains[i].CertExpiry) {
					domains[i].CertExpiry = expiry
					changed = true
				}
			}
		}

		if changed {
			_ = s.store.Domains().Update(ctx, &domains[i])
		}
	}

	return domains, nil
}

func (s *DomainService) Delete(ctx context.Context, id uuid.UUID) error {
	domain, err := s.store.Domains().GetByID(ctx, id)
	if err != nil {
		return err
	}

	if err := s.orch.DeleteRoute(ctx, domain); err != nil {
		return fmt.Errorf("could not delete route from cluster: %w", err)
	}

	return s.store.Domains().Delete(ctx, id)
}

func randomShort(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
