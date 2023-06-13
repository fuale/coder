package coderd

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/xerrors"
	"tailscale.com/tailcfg"

	"github.com/cenkalti/backoff/v4"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"cdr.dev/slog"
	"github.com/coder/coder/coderd"
	agplaudit "github.com/coder/coder/coderd/audit"
	"github.com/coder/coder/coderd/httpapi"
	"github.com/coder/coder/coderd/httpmw"
	"github.com/coder/coder/coderd/rbac"
	"github.com/coder/coder/coderd/schedule"
	"github.com/coder/coder/codersdk"
	"github.com/coder/coder/enterprise/coderd/license"
	"github.com/coder/coder/enterprise/coderd/proxyhealth"
	"github.com/coder/coder/enterprise/derpmesh"
	"github.com/coder/coder/enterprise/replicasync"
	"github.com/coder/coder/enterprise/tailnet"
	"github.com/coder/coder/provisionerd/proto"
	agpltailnet "github.com/coder/coder/tailnet"
)

// New constructs an Enterprise coderd API instance.
// This handler is designed to wrap the AGPL Coder code and
// layer Enterprise functionality on top as much as possible.
func New(ctx context.Context, options *Options) (*API, error) {
	if options.EntitlementsUpdateInterval == 0 {
		options.EntitlementsUpdateInterval = 10 * time.Minute
	}
	if options.Keys == nil {
		options.Keys = Keys
	}
	if options.Options == nil {
		options.Options = &coderd.Options{}
	}
	if options.PrometheusRegistry == nil {
		options.PrometheusRegistry = prometheus.NewRegistry()
	}
	if options.Options.Authorizer == nil {
		options.Options.Authorizer = rbac.NewCachingAuthorizer(options.PrometheusRegistry)
	}
	ctx, cancelFunc := context.WithCancel(ctx)
	api := &API{
		ctx:    ctx,
		cancel: cancelFunc,

		AGPL:    coderd.New(options.Options),
		Options: options,
	}

	api.AGPL.Options.SetUserGroups = api.setUserGroups

	oauthConfigs := &httpmw.OAuth2Configs{
		Github: options.GithubOAuth2Config,
		OIDC:   options.OIDCConfig,
	}
	apiKeyMiddleware := httpmw.ExtractAPIKeyMW(httpmw.ExtractAPIKeyConfig{
		DB:              options.Database,
		OAuth2Configs:   oauthConfigs,
		RedirectToLogin: false,
	})

	deploymentID, err := options.Database.GetDeploymentID(ctx)
	if err != nil {
		return nil, xerrors.Errorf("failed to get deployment ID: %w", err)
	}

	api.AGPL.APIHandler.Group(func(r chi.Router) {
		r.Get("/entitlements", api.serveEntitlements)
		// /regions overrides the AGPL /regions endpoint
		r.Group(func(r chi.Router) {
			r.Use(apiKeyMiddleware)
			r.Get("/regions", api.regions)
		})
		r.Route("/replicas", func(r chi.Router) {
			r.Use(apiKeyMiddleware)
			r.Get("/", api.replicas)
		})
		r.Route("/licenses", func(r chi.Router) {
			r.Use(apiKeyMiddleware)
			r.Post("/", api.postLicense)
			r.Get("/", api.licenses)
			r.Delete("/{id}", api.deleteLicense)
		})
		r.Route("/applications/reconnecting-pty-signed-token", func(r chi.Router) {
			r.Use(apiKeyMiddleware)
			r.Post("/", api.reconnectingPTYSignedToken)
		})
		r.Route("/workspaceproxies", func(r chi.Router) {
			r.Use(
				api.moonsEnabledMW,
			)
			r.Group(func(r chi.Router) {
				r.Use(
					apiKeyMiddleware,
				)
				r.Post("/", api.postWorkspaceProxy)
				r.Get("/", api.workspaceProxies)
			})
			r.Route("/me", func(r chi.Router) {
				r.Use(
					httpmw.ExtractWorkspaceProxy(httpmw.ExtractWorkspaceProxyConfig{
						DB:       options.Database,
						Optional: false,
					}),
				)
				r.Post("/issue-signed-app-token", api.workspaceProxyIssueSignedAppToken)
				r.Post("/register", api.workspaceProxyRegister)
				r.Post("/deregister", api.workspaceProxyDeregister)
			})
			r.Route("/{workspaceproxy}", func(r chi.Router) {
				r.Use(
					apiKeyMiddleware,
					httpmw.ExtractWorkspaceProxyParam(api.Database, deploymentID, api.AGPL.PrimaryWorkspaceProxy),
				)

				r.Get("/", api.workspaceProxy)
				r.Patch("/", api.patchWorkspaceProxy)
				r.Delete("/", api.deleteWorkspaceProxy)
			})
		})
		r.Route("/organizations/{organization}/groups", func(r chi.Router) {
			r.Use(
				apiKeyMiddleware,
				api.templateRBACEnabledMW,
				httpmw.ExtractOrganizationParam(api.Database),
			)
			r.Post("/", api.postGroupByOrganization)
			r.Get("/", api.groupsByOrganization)
			r.Route("/{groupName}", func(r chi.Router) {
				r.Use(
					httpmw.ExtractGroupByNameParam(api.Database),
				)

				r.Get("/", api.groupByOrganization)
			})
		})
		r.Route("/organizations/{organization}/provisionerdaemons", func(r chi.Router) {
			r.Use(
				api.provisionerDaemonsEnabledMW,
				apiKeyMiddleware,
				httpmw.ExtractOrganizationParam(api.Database),
			)
			r.Get("/", api.provisionerDaemons)
			r.Get("/serve", api.provisionerDaemonServe)
		})
		r.Route("/templates/{template}/acl", func(r chi.Router) {
			r.Use(
				api.templateRBACEnabledMW,
				apiKeyMiddleware,
				httpmw.ExtractTemplateParam(api.Database),
			)
			r.Get("/", api.templateACL)
			r.Patch("/", api.patchTemplateACL)
		})
		r.Route("/groups/{group}", func(r chi.Router) {
			r.Use(
				api.templateRBACEnabledMW,
				apiKeyMiddleware,
				httpmw.ExtractGroupParam(api.Database),
			)
			r.Get("/", api.group)
			r.Patch("/", api.patchGroup)
			r.Delete("/", api.deleteGroup)
		})
		r.Route("/workspace-quota", func(r chi.Router) {
			r.Use(
				apiKeyMiddleware,
			)
			r.Route("/{user}", func(r chi.Router) {
				r.Use(httpmw.ExtractUserParam(options.Database, false))
				r.Get("/", api.workspaceQuota)
			})
		})
		r.Route("/appearance", func(r chi.Router) {
			r.Use(
				apiKeyMiddleware,
			)
			r.Get("/", api.appearance)
			r.Put("/", api.putAppearance)
		})
	})

	if len(options.SCIMAPIKey) != 0 {
		api.AGPL.RootHandler.Route("/scim/v2", func(r chi.Router) {
			r.Use(
				api.scimEnabledMW,
			)
			r.Post("/Users", api.scimPostUser)
			r.Route("/Users", func(r chi.Router) {
				r.Get("/", api.scimGetUsers)
				r.Post("/", api.scimPostUser)
				r.Get("/{id}", api.scimGetUser)
				r.Patch("/{id}", api.scimPatchUser)
			})
		})
	}

	meshRootCA := x509.NewCertPool()
	for _, certificate := range options.TLSCertificates {
		for _, certificatePart := range certificate.Certificate {
			certificate, err := x509.ParseCertificate(certificatePart)
			if err != nil {
				return nil, xerrors.Errorf("parse certificate %s: %w", certificate.Subject.CommonName, err)
			}
			meshRootCA.AddCert(certificate)
		}
	}
	// This TLS configuration spoofs access from the access URL hostname
	// assuming that the certificates provided will cover that hostname.
	//
	// Replica sync and DERP meshing require accessing replicas via their
	// internal IP addresses, and if TLS is configured we use the same
	// certificates.
	meshTLSConfig := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: options.TLSCertificates,
		RootCAs:      meshRootCA,
		ServerName:   options.AccessURL.Hostname(),
	}
	api.replicaManager, err = replicasync.New(ctx, options.Logger, options.Database, options.Pubsub, &replicasync.Options{
		ID:           api.AGPL.ID,
		RelayAddress: options.DERPServerRelayAddress,
		RegionID:     int32(options.DERPServerRegionID),
		TLSConfig:    meshTLSConfig,
	})
	if err != nil {
		return nil, xerrors.Errorf("initialize replica: %w", err)
	}
	api.derpMesh = derpmesh.New(options.Logger.Named("derpmesh"), api.DERPServer, meshTLSConfig)

	if api.AGPL.Experiments.Enabled(codersdk.ExperimentMoons) {
		// Proxy health is a moon feature.
		api.ProxyHealth, err = proxyhealth.New(&proxyhealth.Options{
			Interval:   options.ProxyHealthInterval,
			DB:         api.Database,
			Logger:     options.Logger.Named("proxyhealth"),
			Client:     api.HTTPClient,
			Prometheus: api.PrometheusRegistry,
		})
		if err != nil {
			return nil, xerrors.Errorf("initialize proxy health: %w", err)
		}
		go api.ProxyHealth.Run(ctx)
		// Force the initial loading of the cache. Do this in a go routine in case
		// the calls to the workspace proxies hang and this takes some time.
		go api.forceWorkspaceProxyHealthUpdate(ctx)

		// Use proxy health to return the healthy workspace proxy hostnames.
		f := api.ProxyHealth.ProxyHosts
		api.AGPL.WorkspaceProxyHostsFn.Store(&f)
	}

	err = api.updateEntitlements(ctx)
	if err != nil {
		return nil, xerrors.Errorf("update entitlements: %w", err)
	}
	go api.runEntitlementsLoop(ctx)

	return api, nil
}

type Options struct {
	*coderd.Options

	RBAC         bool
	AuditLogging bool
	// Whether to block non-browser connections.
	BrowserOnly bool
	SCIMAPIKey  []byte

	// Used for high availability.
	DERPServerRelayAddress string
	DERPServerRegionID     int

	EntitlementsUpdateInterval time.Duration
	ProxyHealthInterval        time.Duration
	Keys                       map[string]ed25519.PublicKey
}

type API struct {
	AGPL *coderd.API
	*Options

	// ctx is canceled immediately on shutdown, it can be used to abort
	// interruptible tasks.
	ctx    context.Context
	cancel context.CancelFunc

	// Detects multiple Coder replicas running at the same time.
	replicaManager *replicasync.Manager
	// Meshes DERP connections from multiple replicas.
	derpMesh *derpmesh.Mesh
	// ProxyHealth checks the reachability of all workspace proxies.
	ProxyHealth *proxyhealth.ProxyHealth

	entitlementsMu sync.RWMutex
	entitlements   codersdk.Entitlements
}

func (api *API) Close() error {
	api.cancel()
	_ = api.replicaManager.Close()
	_ = api.derpMesh.Close()
	return api.AGPL.Close()
}

func (api *API) updateEntitlements(ctx context.Context) error {
	api.entitlementsMu.Lock()
	defer api.entitlementsMu.Unlock()

	entitlements, err := license.Entitlements(
		ctx, api.Database,
		api.Logger, len(api.replicaManager.AllPrimary()), len(api.GitAuthConfigs), api.Keys, map[codersdk.FeatureName]bool{
			codersdk.FeatureAuditLog:                   api.AuditLogging,
			codersdk.FeatureBrowserOnly:                api.BrowserOnly,
			codersdk.FeatureSCIM:                       len(api.SCIMAPIKey) != 0,
			codersdk.FeatureHighAvailability:           api.DERPServerRelayAddress != "",
			codersdk.FeatureMultipleGitAuth:            len(api.GitAuthConfigs) > 1,
			codersdk.FeatureTemplateRBAC:               api.RBAC,
			codersdk.FeatureExternalProvisionerDaemons: true,
			codersdk.FeatureAdvancedTemplateScheduling: true,
			codersdk.FeatureWorkspaceProxy:             true,
		})
	if err != nil {
		return err
	}

	if entitlements.RequireTelemetry && !api.DeploymentValues.Telemetry.Enable.Value() {
		// We can't fail because then the user couldn't remove the offending
		// license w/o a restart.
		//
		// We don't simply append to entitlement.Errors since we don't want any
		// enterprise features enabled.
		api.entitlements.Errors = []string{
			"License requires telemetry but telemetry is disabled",
		}
		api.Logger.Error(ctx, "license requires telemetry enabled")
		return nil
	}

	featureChanged := func(featureName codersdk.FeatureName) (changed bool, enabled bool) {
		if api.entitlements.Features == nil {
			return true, entitlements.Features[featureName].Enabled
		}
		oldFeature := api.entitlements.Features[featureName]
		newFeature := entitlements.Features[featureName]
		if oldFeature.Enabled != newFeature.Enabled {
			return true, newFeature.Enabled
		}
		return false, newFeature.Enabled
	}

	if changed, enabled := featureChanged(codersdk.FeatureAuditLog); changed {
		auditor := agplaudit.NewNop()
		if enabled {
			auditor = api.AGPL.Options.Auditor
		}
		api.AGPL.Auditor.Store(&auditor)
	}

	if changed, enabled := featureChanged(codersdk.FeatureBrowserOnly); changed {
		var handler func(rw http.ResponseWriter) bool
		if enabled {
			handler = api.shouldBlockNonBrowserConnections
		}
		api.AGPL.WorkspaceClientCoordinateOverride.Store(&handler)
	}

	if changed, enabled := featureChanged(codersdk.FeatureTemplateRBAC); changed {
		if enabled {
			committer := committer{Database: api.Database}
			ptr := proto.QuotaCommitter(&committer)
			api.AGPL.QuotaCommitter.Store(&ptr)
		} else {
			api.AGPL.QuotaCommitter.Store(nil)
		}
	}

	if changed, enabled := featureChanged(codersdk.FeatureAdvancedTemplateScheduling); changed {
		if enabled {
			store := &enterpriseTemplateScheduleStore{}
			ptr := schedule.TemplateScheduleStore(store)
			api.AGPL.TemplateScheduleStore.Store(&ptr)
		} else {
			store := schedule.NewAGPLTemplateScheduleStore()
			api.AGPL.TemplateScheduleStore.Store(&store)
		}
	}

	if changed, enabled := featureChanged(codersdk.FeatureHighAvailability); changed {
		coordinator := agpltailnet.NewCoordinator(api.Logger, api.AGPL.DERPMap)
		if enabled {
			haCoordinator, err := tailnet.NewCoordinator(api.Logger, api.Pubsub, api.AGPL.DERPMap)
			if err != nil {
				api.Logger.Error(ctx, "unable to set up high availability coordinator", slog.Error(err))
				// If we try to setup the HA coordinator and it fails, nothing
				// is actually changing.
				changed = false
			} else {
				coordinator = haCoordinator
			}

			api.replicaManager.SetCallback(func() {
				addresses := make([]string, 0)
				for _, replica := range api.replicaManager.Regional() {
					addresses = append(addresses, replica.RelayAddress)
				}
				api.derpMesh.SetAddresses(addresses, false)
				_ = api.updateEntitlements(ctx)
			})
		} else {
			api.derpMesh.SetAddresses([]string{}, false)
			api.replicaManager.SetCallback(func() {
				// If the amount of replicas change, so should our entitlements.
				// This is to display a warning in the UI if the user is unlicensed.
				_ = api.updateEntitlements(ctx)
			})
		}

		// Recheck changed in case the HA coordinator failed to set up.
		if changed {
			oldCoordinator := *api.AGPL.TailnetCoordinator.Swap(&coordinator)
			err := oldCoordinator.Close()
			if err != nil {
				api.Logger.Error(ctx, "close old tailnet coordinator", slog.Error(err))
			}
		}
	}

	if changed, enabled := featureChanged(codersdk.FeatureWorkspaceProxy); changed {
		if enabled {
			fn := derpMapper(api.Logger, api.ProxyHealth)
			api.AGPL.DERPMapper.Store(&fn)
		} else {
			api.AGPL.DERPMapper.Store(nil)
		}
	}

	api.entitlements = entitlements

	return nil
}

// getProxyDERPStartingRegionID returns the starting region ID that should be
// used for workspace proxies. A proxy's actual region ID is the return value
// from this function + it's RegionID field.
//
// Two ints are returned, the first is the starting region ID for proxies, and
// the second is the maximum region ID that already exists in the DERP map.
func getProxyDERPStartingRegionID(derpMap *tailcfg.DERPMap) (sID int, mID int) {
	maxRegionID := 0
	for _, region := range derpMap.Regions {
		if region.RegionID > maxRegionID {
			maxRegionID = region.RegionID
		}
	}
	if maxRegionID < 0 {
		maxRegionID = 0
	}

	// Round to the nearest 10,000 with a sufficient buffer of at least 2,000.
	const roundStartingRegionID = 10_000
	const startingRegionIDBuffer = 2_000
	startingRegionID := maxRegionID + startingRegionIDBuffer
	startingRegionID = int(math.Ceil(float64(startingRegionID)/roundStartingRegionID) * roundStartingRegionID)
	if startingRegionID < roundStartingRegionID {
		startingRegionID = roundStartingRegionID
	}

	return startingRegionID, maxRegionID
}

var (
	lastDerpConflictMutex sync.Mutex
	lastDerpConflictLog   time.Time
)

func derpMapper(logger slog.Logger, proxyHealth *proxyhealth.ProxyHealth) func(*tailcfg.DERPMap) *tailcfg.DERPMap {
	return func(derpMap *tailcfg.DERPMap) *tailcfg.DERPMap {
		derpMap = derpMap.Clone()

		// Find the starting region ID that we'll use for proxies. This must be
		// deterministic based on the derp map.
		startingRegionID, largestRegionID := getProxyDERPStartingRegionID(derpMap)
		if largestRegionID >= 1<<32 {
			// Enforce an upper bound on the region ID. This shouldn't be hit in
			// practice, but it's a good sanity check.
			lastDerpConflictMutex.Lock()
			shouldLog := lastDerpConflictLog.IsZero() || time.Since(lastDerpConflictLog) > time.Minute
			if shouldLog {
				lastDerpConflictLog = time.Now()
			}
			lastDerpConflictMutex.Unlock()
			if shouldLog {
				logger.Warn(
					context.Background(),
					"existing DERP region IDs are too large, proxy region IDs will not be populated in the derp map. Please ensure that all DERP region IDs are less than 2^32.",
					slog.F("largest_region_id", largestRegionID),
					slog.F("max_region_id", 1<<32-1),
				)
				return derpMap
			}
		}

		// Add all healthy proxies to the DERP map.
		statusMap := proxyHealth.HealthStatus()
	statusLoop:
		for _, status := range statusMap {
			if status.Status != proxyhealth.Healthy || !status.Proxy.DerpEnabled {
				// Only add healthy proxies with DERP enabled to the DERP map.
				continue
			}

			u, err := url.Parse(status.Proxy.Url)
			if err != nil {
				// Not really any need to log, the proxy should be unreachable
				// anyways and filtered out by the above condition.
				continue
			}
			port := u.Port()
			if port == "" {
				port = "80"
				if u.Scheme == "https" {
					port = "443"
				}
			}
			portInt, err := strconv.Atoi(port)
			if err != nil {
				// Not really any need to log, the proxy should be unreachable
				// anyways and filtered out by the above condition.
				continue
			}

			// Sanity check that the region ID and code is unique.
			//
			// This should be impossible to hit as the IDs are enforced to be
			// unique by the database and the computed ID is greater than any
			// existing ID in the DERP map.
			regionID := startingRegionID + int(status.Proxy.RegionID)
			regionCode := fmt.Sprintf("coder_%s", strings.ToLower(status.Proxy.Name))
			for _, r := range derpMap.Regions {
				if r.RegionID == regionID || r.RegionCode == regionCode {
					// Log a warning if we haven't logged one in the last
					// minute.
					lastDerpConflictMutex.Lock()
					shouldLog := lastDerpConflictLog.IsZero() || time.Since(lastDerpConflictLog) > time.Minute
					if shouldLog {
						lastDerpConflictLog = time.Now()
					}
					lastDerpConflictMutex.Unlock()
					if shouldLog {
						logger.Warn(context.Background(),
							"proxy region ID or code conflict, ignoring workspace proxy for DERP map. Please change the flags on the affected proxy to use a different region ID and code.",
							slog.F("proxy_id", status.Proxy.ID),
							slog.F("proxy_name", status.Proxy.Name),
							slog.F("proxy_display_name", status.Proxy.DisplayName),
							slog.F("proxy_url", status.Proxy.Url),
							slog.F("proxy_region_id", status.Proxy.RegionID),
							slog.F("proxy_computed_region_id", regionID),
							slog.F("proxy_computed_region_code", regionCode),
						)
					}

					continue statusLoop
				}
			}

			derpMap.Regions[regionID] = &tailcfg.DERPRegion{
				// EmbeddedRelay ONLY applies to the primary.
				EmbeddedRelay: false,
				RegionID:      regionID,
				RegionCode:    regionCode,
				RegionName:    status.Proxy.Name,
				Nodes: []*tailcfg.DERPNode{{
					Name:      fmt.Sprintf("%da", regionID),
					RegionID:  regionID,
					HostName:  u.Hostname(),
					DERPPort:  portInt,
					STUNPort:  -1,
					ForceHTTP: u.Scheme == "http",
				}},
			}
		}

		return derpMap
	}
}

// @Summary Get entitlements
// @ID get-entitlements
// @Security CoderSessionToken
// @Produce json
// @Tags Enterprise
// @Success 200 {object} codersdk.Entitlements
// @Router /entitlements [get]
func (api *API) serveEntitlements(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	api.entitlementsMu.RLock()
	entitlements := api.entitlements
	api.entitlementsMu.RUnlock()
	httpapi.Write(ctx, rw, http.StatusOK, entitlements)
}

func (api *API) runEntitlementsLoop(ctx context.Context) {
	eb := backoff.NewExponentialBackOff()
	eb.MaxElapsedTime = 0 // retry indefinitely
	b := backoff.WithContext(eb, ctx)
	updates := make(chan struct{}, 1)
	subscribed := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// pass
		}
		if !subscribed {
			cancel, err := api.Pubsub.Subscribe(PubsubEventLicenses, func(_ context.Context, _ []byte) {
				// don't block.  If the channel is full, drop the event, as there is a resync
				// scheduled already.
				select {
				case updates <- struct{}{}:
					// pass
				default:
					// pass
				}
			})
			if err != nil {
				api.Logger.Warn(ctx, "failed to subscribe to license updates", slog.Error(err))
				select {
				case <-ctx.Done():
					return
				case <-time.After(b.NextBackOff()):
				}
				continue
			}
			// nolint: revive
			defer cancel()
			subscribed = true
			api.Logger.Debug(ctx, "successfully subscribed to pubsub")
		}

		api.Logger.Debug(ctx, "syncing licensed entitlements")
		err := api.updateEntitlements(ctx)
		if err != nil {
			api.Logger.Warn(ctx, "failed to get feature entitlements", slog.Error(err))
			time.Sleep(b.NextBackOff())
			continue
		}
		b.Reset()
		api.Logger.Debug(ctx, "synced licensed entitlements")

		select {
		case <-ctx.Done():
			return
		case <-time.After(api.EntitlementsUpdateInterval):
			continue
		case <-updates:
			api.Logger.Debug(ctx, "got pubsub update")
			continue
		}
	}
}

func (api *API) Authorize(r *http.Request, action rbac.Action, object rbac.Objecter) bool {
	return api.AGPL.HTTPAuth.Authorize(r, action, object)
}
