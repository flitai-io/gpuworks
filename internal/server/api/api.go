// Package api wires the REST/SSE surface (chi) and maps store entities to DTOs.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// Router builds the top-level HTTP handler.
func Router(d Deps, policies ...auth.SessionPolicy) http.Handler {
	policy := auth.SessionPolicy{}.Normalized()
	if len(policies) > 0 {
		policy = policies[0].Normalized()
	}
	a := &apiHandlers{d: d, sessionPolicy: policy, benchmarkExecutions: map[uint]*benchmarkExecution{}}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(a.countUsage)
	a.aiAsks.limit, a.mcpCalls.limit = aiAsksPerUser, mcpCallsPerUser
	r.Use(a.securityHeaders)
	r.NotFound(a.routingError(http.StatusNotFound))
	r.MethodNotAllowed(a.routingError(http.StatusMethodNotAllowed))

	// Agent long-connection endpoint (WS upgrade → yamux). Authentication is
	// performed by the hub's per-node-token handshake, not management JWT.
	r.Get("/api/v1/agent/connect", d.AgentConnect)

	// Install script (curl | sh, with an explicit per-node token).
	if d.InstallSh != nil {
		r.Get("/install.sh", d.InstallSh)
	}

	// Kubernetes manifest, fetched by `kubectl apply -f` with the cluster's
	// enrollment token in place of a login.
	r.Get("/kube/manifest.yaml", a.kubeManifestByToken)

	// Data plane: OpenAI-compatible /v1. ProxyV1 owns API-key auth for every
	// endpoint, including /v1/models and the batch files.
	if d.ProxyV1 != nil {
		r.Handle("/v1/*", d.ProxyV1)
	}

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/login", a.login)
		// A Kubernetes node's agent authenticates with the enrollment token.
		r.Post("/kube/enroll", a.kubeEnroll)
		r.Post("/auth/logout", a.logout)
		// Open to the sign-in page; it reveals nothing (see prefetchUpdates).
		r.Post("/updates/prefetch", a.prefetchUpdates)
		// Open to the sign-in page, which shows the product name and logo.
		r.Get("/branding", a.getBranding)
		r.With(a.managementAuth).Get("/auth/me", a.me)
		r.With(a.managementAuth, sessionOnly).Post("/auth/activity", a.activity)
		r.With(a.managementAuth, sessionOnly).Post("/auth/password", a.changePassword)
		// Platform tokens are managed from a login session only, so no token
		// can mint itself a wider one.
		r.Group(func(r chi.Router) {
			r.Use(a.managementAuth, sessionOnly)
			r.Get("/platform-tokens", a.listPlatformTokens)
			r.Post("/platform-tokens", a.createPlatformToken)
			r.Delete("/platform-tokens/{id}", a.deletePlatformToken)
		})
		// MCP serves read-only tools, so a read token may POST to it.
		r.With(a.managementAuth).Post("/mcp", a.mcp)
		r.Group(func(r chi.Router) {
			r.Use(a.managementAuth, readTokensRead)
			r.Use(compressJSON)
			r.Get("/healthz", a.health)
			r.Get("/peer-transfers", a.peerTransferSnapshot)
			r.Get("/engines", a.engines)
			r.Get("/nodes", a.listNodes)
			r.Get("/nodes/facets", a.nodeFacets)
			r.Get("/dashboard/summary", a.dashboardSummary)
			r.With(a.managementAdmin).Post("/nodes", a.createNode)
			r.Get("/kube/clusters", a.listKubeClusters)
			r.Get("/kube/defaults", a.kubeDefaults)
			r.With(a.managementAdmin).Post("/kube/clusters", a.createKubeCluster)
			r.With(a.managementAdmin).Get("/kube/clusters/{uuid}/manifest", a.kubeClusterManifest)
			r.With(a.managementAdmin).Post("/kube/clusters/{uuid}/revoke", a.revokeKubeCluster)
			r.With(a.managementAdmin).Put("/kube/clusters/{uuid}/pools", a.setKubeClusterPools)
			r.With(a.managementAdmin).Delete("/kube/clusters/{uuid}", a.deleteKubeCluster)
			r.With(a.managementAdmin).Post("/kube/clusters/{uuid}/apply", a.applyKubeCluster)
			r.With(a.managementAdmin).Post("/kube/clusters/{uuid}/remove", a.removeKubeCluster)
			r.Get("/agent/rejects", a.listRejects)
			r.Get("/agent/upgrade-errors", a.listUpgradeErrors)
			r.With(a.managementAdmin).Post("/agent-upgrades/preview", a.previewAgentUpgrade)
			r.With(a.managementAdmin).Post("/agent-upgrades", a.createAgentUpgrade)
			r.Get("/agent-upgrades", a.listAgentUpgrades)
			r.Get("/agent-upgrades/summary", a.agentFleetSummary)
			r.Get("/agent-upgrades/{id}", a.getAgentUpgrade)
			r.Get("/agent-upgrades/{id}/items", a.agentUpgradeItems)
			r.With(a.managementAdmin).Post("/agent-upgrades/{id}/stop", a.stopAgentUpgrade)
			r.With(a.managementAdmin).Post("/agent-upgrades/{id}/continue-preview", a.continueAgentUpgradePreview)
			r.With(a.managementAdmin).Post("/agent-upgrades/{id}/items/{uuid}/release", a.releaseAgentUpgrade)
			r.With(a.managementAdmin).Post("/nodes/precheck", a.precheckNode)
			r.With(a.managementAdmin).Post("/nodes/install", a.installNode)
			r.Get("/nodes/{uuid}", a.getNode)
			r.Get("/nodes/{uuid}/fs", a.nodeFSList)
			r.Post("/nodes/{uuid}/doctor", a.nodeDoctor)
			r.Patch("/nodes/{uuid}/runtime", a.updateNodeRuntime)
			r.Patch("/nodes/{uuid}/accelerator", a.updateNodeAccelerator)
			r.Post("/nodes/{uuid}/upgrade", a.upgradeNode)
			r.Put("/nodes/{uuid}/labels", a.updateNodeLabels)
			r.Put("/nodes/{uuid}/scheduling", a.setNodeScheduling)
			r.Put("/nodes/{uuid}/gpus/scheduling", a.setCardScheduling)
			r.Post("/nodes/{uuid}/gpus/recheck", a.recheckCard)
			r.Get("/nodes/{uuid}/health-checks", a.nodeHealthChecks)
			r.Get("/nodes/{uuid}/health-checks/detail", a.nodeHealthCheckDetail)
			r.Get("/nodes/{uuid}/health-records", a.nodeHealthRecords)
			r.Get("/nodes/{uuid}/replicas", a.nodeReplicas)
			r.Post("/nodes/{uuid}/evict", a.evictNode)
			r.Patch("/nodes/{uuid}/name", a.renameNode)
			r.Get("/nodes/{uuid}/network", a.nodeNetworkRemoved)
			r.Get("/nodes/{uuid}/pull-proxy", a.getNodePullProxy)
			r.Get("/nodes/{uuid}/networks", a.getNodeNetworks)
			r.With(a.managementAdmin).Put("/nodes/{uuid}/management-roles", a.putNodeManagementRoles)
			r.With(a.managementAdmin).Patch("/nodes/management-roles", a.patchManagementRoles)
			r.With(a.managementAdmin).Put("/nodes/{uuid}/network", a.nodeNetworkRemoved)
			r.With(a.managementAdmin).Post("/nodes/{uuid}/runtime/restart", a.restartNodeRuntime)
			r.Get("/ssh-key", a.getPlatformSSHKey)
			r.With(a.managementAdmin).Put("/ssh-key", a.importPlatformSSHKey)
			r.With(a.managementAdmin).Post("/ssh-key/rotate", a.rotatePlatformSSHKey)
			r.Get("/nodes/{uuid}/ssh", a.getNodeSSH)
			r.With(a.managementAdmin).Put("/nodes/{uuid}/ssh", a.putNodeSSH)
			r.With(a.managementAdmin).Delete("/nodes/{uuid}/ssh", a.deleteNodeSSH)
			r.With(a.managementAdmin).Post("/nodes/{uuid}/recovery/probe", a.probeNodeRecovery)
			r.With(a.managementAdmin).Post("/nodes/{uuid}/connection-diagnostics", a.nodeConnectionDiagnostics)
			r.With(a.managementAdmin).Post("/nodes/{uuid}/token/regenerate", a.regenerateNodeToken)
			r.Delete("/nodes/{uuid}", a.deleteNode)
			r.Get("/events", a.events) // SSE stream of change events

			// Models.
			r.Get("/models", a.listModels)
			r.Post("/models", a.createModel)
			r.Post("/models/quick-register", a.quickRegisterModel)
			r.Post("/models/local/preview", a.previewLocalModel)
			r.Post("/models/local", a.registerLocalModel)
			r.Post("/models/preview-url", a.previewURLModel)
			r.Post("/models/register-url", a.registerURLModel)
			r.Get("/models/{id}/logo", a.getModelLogo)
			r.Get("/models/{id}", a.getModel)
			r.With(a.managementAdmin).Delete("/models/{id}", a.deleteModel)
			r.Patch("/models/{id}", a.updateModel)
			r.Patch("/models/{id}/inference-config", a.updateModelInferenceConfig)
			r.Post("/models/{id}/metadata/refresh", a.refreshModelMetadata)
			r.Post("/models/{id}/download", a.downloadModel)
			r.Post("/models/{id}/download/cancel", a.cancelModelDownload)
			r.Post("/models/{id}/copy", a.copyModel)
			r.Post("/models/{id}/nodes/{uuid}/cache/retry", a.retryModelCache)
			r.With(a.managementAdmin).Delete("/models/{id}/storages/{storage_id}", a.deleteModelInStorage)
			r.Post("/models/{id}/register-path", a.registerModelPath)
			r.Post("/models/{id}/nodes/{uuid}/sync", a.syncModelToNode)
			r.Post("/models/{id}/nodes/{uuid}/sync/probe", a.probeModelToNode)
			r.Post("/models/{id}/nodes/{uuid}/sync/cancel", a.cancelModelSync)
			r.With(a.managementAdmin).Delete("/models/{id}/nodes/{uuid}", a.evictModelNode)

			// Image catalog and storage policies.
			r.Get("/images", a.listImages)
			r.Get("/images/versions", a.listImageVersions)
			r.Get("/images/engines", a.listImageEngines)
			r.Get("/images/community/repos", a.listCommunityRepos)
			r.Get("/images/community/browse", a.browseCommunityImages)
			r.With(a.managementAdmin).Patch("/images/versions/{id}", a.patchImageVersion)
			r.Get("/images/{id}", a.getImage)
			r.With(a.managementAdmin).Patch("/images/{id}", a.patchImageSeries)
			r.With(a.managementAdmin).Delete("/images/{id}", a.deleteImage)
			r.With(a.managementAdmin).Delete("/image-series/{id}", a.deleteImageSeries)
			r.With(a.managementAdmin).Post("/images/import", a.importImages)
			r.With(a.managementAdmin).Post("/images/sync", a.syncImagesManifest)
			r.With(a.managementAdmin).Post("/images/preview-archive-url", a.previewImageArchiveURL)
			r.Post("/images/{id}/nodes/{uuid}/sync", a.syncImageToNode)
			r.Post("/images/{id}/nodes/{uuid}/sync/probe", a.probeImageToNode)
			r.Post("/images/{id}/nodes/{uuid}/sync/retry", a.retryImageToNode)
			r.Post("/images/{id}/nodes/{uuid}/cancel", a.cancelImageToNode)
			r.With(a.managementAdmin).Delete("/images/{id}/nodes/{uuid}", a.removeImageFromNode)
			r.With(a.managementAdmin).Post("/images/{id}/nodes/{uuid}/inspect", a.inspectImageOnNode)
			r.Get("/storages", a.listStorages)
			r.Get("/storages/usage", a.storageUsage)
			r.Get("/storages/settings", a.getStorageSettings)
			r.With(a.managementAdmin).Patch("/storages/settings", a.patchStorageSettings)
			r.With(a.managementAdmin).Post("/storages", a.createStorage)
			r.With(a.managementAdmin).Post("/storages/probe", a.probeStorage)
			r.Get("/storages/{id}", a.getStorage)
			r.With(a.managementAdmin).Patch("/storages/{id}", a.patchStorage)
			r.With(a.managementAdmin).Patch("/storages/{id}/node-overrides", a.patchStorageNodeOverrides)
			r.With(a.managementAdmin).Post("/storages/{id}/cache/reconcile", a.reconcileStorageCache)
			r.With(a.managementAdmin).Delete("/storages/{id}", a.deleteStorage)
			r.Get("/storages/{id}/control-planes", a.storageControlPlanes)
			r.Get("/storages/{id}/model-files", a.storageModelFiles)
			r.With(a.managementAdmin).Post("/storages/{id}/scan", a.scanStorage)
			r.With(a.managementAdmin).Post("/storages/{id}/import", a.importStorageModels)
			r.With(a.managementAdmin).Delete("/storages/{id}/cache", a.clearStorageCache)
			// Storage files (ADR 0053): browsing included, admin only.
			r.With(a.managementAdmin).Get("/storages/{id}/files", a.listStorageFiles)
			r.With(a.managementAdmin).Delete("/storages/{id}/files", a.deleteStorageFile)
			r.With(a.managementAdmin).Get("/storages/{id}/files/download", a.downloadStorageFile)
			r.With(a.managementAdmin).Put("/storages/{id}/files/upload", a.uploadStorageFile)
			r.With(a.managementAdmin).Post("/storages/{id}/files/mkdir", a.mkdirStorageFile)
			r.With(a.managementAdmin).Post("/storages/{id}/files/rename", a.renameStorageFile)

			// Inference templates and deployments.
			r.Get("/inference-templates", a.listInferenceTemplates)
			r.Get("/inference-templates/index", a.listInferenceTemplateIndex)
			r.Get("/inference-templates/organizations", a.listInferenceTemplateOrganizations)
			r.Get("/inference-templates/chips", a.listTemplateChips)
			r.Get("/inference-templates/my-chips", a.listDetectedTemplateChips)
			r.Get("/inference-templates/schema", a.inferenceTemplateSchema)
			r.Get("/inference-templates/summary", a.summarizeInferenceTemplates)
			r.Get("/inference-templates/weight-models", a.listTemplateWeightModels)
			r.With(a.managementAdmin).Get("/inference-templates/export/{digest}", a.exportInferenceTemplatePackage)
			r.Get("/inference-templates/model/*", a.getInferenceTemplate)
			r.Get("/inference-templates/yaml", a.inferenceTemplateYAML)
			r.With(a.managementAdmin).Post("/inference-templates/clone", a.cloneInferenceTemplate)
			r.With(a.managementAdmin).Post("/inference-templates/import", a.importInferenceTemplates)
			r.With(a.managementAdmin).Post("/inference-templates/evaluate", a.evaluateInferenceTemplates)
			r.With(a.managementAdmin).Delete("/inference-templates/spec", a.deleteInferenceTemplateSpec)
			r.With(a.managementAdmin).Post("/inference-templates/sync", a.syncInferenceTemplates)
			r.With(a.managementAdmin).Post("/inference-templates/official", a.uploadOfficialInferenceCatalog)
			r.Get("/deployments", a.listDeployments)
			r.Get("/routing-policies", a.routingPolicies)
			r.Post("/deployments/preflight", a.distributedPreflight)
			r.Post("/deployments", a.createDeployment)
			r.Get("/deployments/{id}", a.getDeployment)
			r.Get("/deployments/{id}/candidates", a.deploymentCandidates)
			r.Get("/deployments/{id}/draft", a.getDeploymentDraft)
			r.Put("/deployments/{id}", a.editDeployment)
			r.Put("/deployments/{id}/requirements", a.setDeploymentRequirements)
			r.Put("/deployments/{id}/routing", a.setRoutingPolicy)
			r.Put("/deployments/{id}/health-check", a.setHealthCheck)
			r.Put("/deployments/{id}/tokenize", a.setTokenize)
			r.Put("/deployments/{id}/dgpu", a.setDeploymentDGPUQuota)
			r.Post("/deployments/{id}/clone", a.cloneDeployment)
			r.Post("/deployments/{id}/catalog-template", a.exportCatalogTemplate)
			r.Post("/deployments/{id}/stop", a.stopDeployment)
			r.Post("/deployments/{id}/start", a.startDeployment)
			r.Put("/deployments/{id}/pd", a.updatePD)
			r.Put("/deployments/{id}/replicas", a.scaleReplicas)
			r.Post("/deployments/{id}/restart", a.restartDeployment)
			r.Post("/deployments/{id}/units/retry", a.retryUnit)
			r.Post("/deployments/{id}/units/suggest", a.suggestUnit)
			r.Delete("/deployments/{id}", a.deleteDeployment)
			r.Post("/deployments/{id}/sleep", a.sleepDeployment)
			r.Post("/deployments/{id}/wake", a.wakeDeployment)
			r.Delete("/deployments/{id}/wake-waiting", a.endWakeWaiting)
			r.Post("/deployments/{id}/wake-on-request", a.releaseSleepHold)
			r.Get("/deployments/{id}/schedule", a.listSchedule)
			r.Post("/deployments/{id}/schedule", a.createSchedule)
			r.Put("/deployments/{id}/schedule/{sid}", a.updateSchedule)
			r.Delete("/deployments/{id}/schedule/{sid}", a.deleteSchedule)
			r.Put("/deployments/{id}/node-replacement", a.setNodeReplacement)
			r.Put("/deployments/{id}/yield-policy", a.setYieldPolicy)
			r.Put("/deployments/{id}/placement-mode", a.setPlacementMode)
			// A deployment's own timeline, including scheduling outcomes
			// recorded before any instance existed.
			r.Get("/deployments/{id}/events", a.deploymentEvents)
			r.Post("/evaluate", a.evaluate)
			r.Get("/benchmarks", a.listBenchmarks)
			r.Get("/benchmarks/meta", a.getBenchmarkMeta)
			r.Post("/benchmarks", a.createBenchmark)
			r.Post("/benchmarks/probe", a.probeBenchmark)
			r.Post("/benchmarks/models", a.listBenchmarkModels)
			r.Post("/benchmarks/datasets/import", a.importBenchmarkDataset)
			r.Post("/benchmarks/tokenizers/import", a.importBenchmarkTokenizer)
			r.Get("/benchmarks/{id}", a.getBenchmark)
			r.Get("/benchmarks/{id}/curve", a.getBenchmarkCurve)
			r.Get("/benchmarks/{id}/report", a.downloadBenchmarkReport)
			r.Delete("/benchmarks/{id}", a.deleteBenchmark)
			r.Get("/benchmarks/{id}/points/{seq}/detail", a.downloadBenchmarkDetail)
			r.Get("/benchmarks/{id}/points/{seq}/timeline", a.getBenchmarkRequestTimeline)
			r.Post("/benchmarks/{id}/cancel", a.cancelBenchmark)

			// Instances.
			r.Get("/instances", a.listInstances)
			r.Get("/instances/{uuid}", a.getInstance)
			r.Get("/instances/{uuid}/launch-facts", a.getInstanceLaunchFacts)
			r.Get("/instances/{uuid}/events", a.instanceEvents)
			r.Get("/instances/{uuid}/logs", a.instanceLogs) // SSE
			r.Post("/instances/{uuid}/restart", a.restartInstance)
			r.Post("/instances/{uuid}/sleep", a.sleepInstance)
			r.Post("/instances/{uuid}/wake", a.wakeInstance)

			// API keys.
			r.Get("/apikeys", a.listAPIKeys)
			r.Post("/apikeys", a.createAPIKey)
			r.Delete("/apikeys/{prefix}", a.deleteAPIKey)
			r.Put("/apikeys/{prefix}/scope", a.setAPIKeyScope)
			r.Get("/mcp/status", a.mcpStatus)
			r.With(a.managementAdmin).Put("/mcp/settings", a.putMCPSettings)

			// Cloud providers.
			r.Get("/providers/kinds", a.providerKinds)
			r.Get("/providers", a.listProviders)
			r.Post("/providers", a.addProvider)
			r.With(sessionOnly).Get("/providers/{id}", a.getProvider)
			r.Put("/providers/{id}", a.updateProvider)
			r.Delete("/providers/{id}", a.deleteProvider)
			r.Post("/providers/{id}/sync", a.syncProvider)
			r.Get("/providers/{id}/clusters", a.providerClusters)
			r.Post("/providers/{id}/instances", a.createProviderInstance)
			r.Get("/providers/{id}/instances", a.providerInstances)
			r.Delete("/providers/{id}/instances/{externalID}", a.deleteProviderInstance)
			r.Post("/providers/{id}/instances/{externalID}/power", a.powerProviderInstance)
			r.Post("/providers/{id}/instances/{externalID}/resize", a.resizeProviderInstance)
			r.Post("/providers/{id}/install", a.installAgentOnInstance)

			// Networks.
			r.Get("/networks", a.listNetworks)
			r.With(a.managementAdmin).Post("/networks", a.createNetwork)
			r.Get("/networks/{id}", a.getNetwork)
			r.With(a.managementAdmin).Put("/networks/{id}", a.updateNetwork)
			r.With(a.managementAdmin).Delete("/networks/{id}", a.deleteNetwork)
			r.With(a.managementAdmin).Post("/networks/{id}/attachments", a.createNetworkAttachment)
			r.With(a.managementAdmin).Post("/networks/{id}/attachments/batch", a.attachNetworkBatch)
			r.With(a.managementAdmin).Post("/networks/{id}/hcas", a.editNetworkHCAs)
			r.With(a.managementAdmin).Put("/networks/{id}/attachments/{attachmentID}", a.updateNetworkAttachment)
			r.With(a.managementAdmin).Delete("/networks/{id}/attachments/{attachmentID}", a.deleteNetworkAttachment)
			r.With(a.managementAdmin).Post("/networks/{id}/tests", a.startNetworkTest)
			r.With(a.managementAdmin).Post("/networks/{id}/tests/estimate", a.estimateNetworkTest)

			// Web terminal. Admin-only in both branches: a host terminal is
			// a root shell on a GPU node, and the container branch is not worth a
			// separate per-deployment authorization model yet.
			r.With(a.managementAdmin).Get("/nodes/{uuid}/terminal", a.nodeTerminal)
			r.With(a.managementAdmin).Get("/nodes/{uuid}/terminal/settings", a.terminalSettings)
			r.With(a.managementAdmin).Get("/terminal/sessions", a.terminalSessionsList)

			// Pull settings: where nodes get images and models from.
			r.Get("/pull/source-rules", a.listPullSourceRules)
			r.With(a.managementAdmin).Post("/pull/source-rules", a.createPullSourceRule)
			r.With(a.managementAdmin).Put("/pull/source-rules/order", a.orderPullSourceRules)
			r.With(a.managementAdmin).Post("/pull/source-rules/test", a.testSourceRule)
			r.With(a.managementAdmin).Put("/pull/source-rules/{id}", a.updatePullSourceRule)
			r.With(a.managementAdmin).Delete("/pull/source-rules/{id}", a.deletePullSourceRule)
			r.Get("/pull/metadata-sources", a.metadataSources)
			r.Get("/pull/proxies", a.listPullProxies)
			r.With(a.managementAdmin).Post("/pull/proxies", a.createPullProxy)
			r.With(a.managementAdmin).Put("/pull/proxies/order", a.orderPullProxies)
			r.With(a.managementAdmin).Put("/pull/proxies/{id}", a.updatePullProxy)
			r.With(a.managementAdmin).Delete("/pull/proxies/{id}", a.deletePullProxy)
			r.Get("/pull/sync-groups", a.listSyncGroups)
			r.With(a.managementAdmin).Post("/pull/sync-groups", a.createSyncGroup)
			r.With(a.managementAdmin).Put("/pull/sync-groups/{id}", a.updateSyncGroup)
			r.With(a.managementAdmin).Delete("/pull/sync-groups/{id}", a.deleteSyncGroup)
			r.With(a.managementAdmin).Get("/pull/credentials", a.listCredentials)
			r.With(a.managementAdmin).Post("/pull/credentials", a.createCredential)
			r.With(a.managementAdmin).Put("/pull/credentials/{id}", a.updateCredential)
			r.With(a.managementAdmin).Delete("/pull/credentials/{id}", a.deleteCredential)
			r.With(a.managementAdmin).Post("/pull/credentials/{id}/test", a.testCredential)
			r.With(a.managementAdmin).Get("/pull/registry-credentials", a.registryCredentialsRemoved)
			r.With(a.managementAdmin).Post("/pull/registry-credentials", a.registryCredentialsRemoved)
			r.With(a.managementAdmin).Put("/pull/registry-credentials/{id}", a.registryCredentialsRemoved)
			r.With(a.managementAdmin).Delete("/pull/registry-credentials/{id}", a.registryCredentialsRemoved)
			r.With(a.managementAdmin).Post("/pull/registry-credentials/{id}/test", a.registryCredentialsRemoved)

			// Settings (network/accel presets, etc.).
			r.Get("/settings", a.getSettings)
			r.With(a.managementAdmin).Put("/settings/{key}", a.putSetting)
			r.Get("/telemetry/export", a.getTelemetryExport)
			r.With(a.managementAdmin).Put("/telemetry/export", a.putTelemetryExport)
			r.With(a.managementAdmin).Post("/telemetry/export/test", a.testTelemetryExport)
			r.Get("/security/settings", a.getSecuritySettings)
			r.With(a.managementAdmin).Patch("/security/settings", a.patchSecuritySettings)
			r.With(a.managementAdmin).Patch("/branding", a.patchBranding)
			r.Get("/data-retention", a.getDataRetention)
			r.With(a.managementAdmin).Put("/data-retention", a.putDataRetention)
			r.Get("/data-retention/status", a.getDataRetentionStatus)
			r.Get("/analytics/usage", analyticsRoute(a, Analytics.Usage))
			r.Get("/analytics/quality", analyticsRoute(a, Analytics.Quality))
			r.Get("/analytics/gpus", analyticsRoute(a, Analytics.GPUs))
			r.Get("/analytics/gpu-time", analyticsRoute(a, Analytics.GPUTime))
			r.Get("/analytics/key-usage", a.getKeyUsage)
			r.With(a.managementAdmin).Get("/updates", a.getUpdates)
			r.With(a.managementAdmin).Put("/updates/channel", a.putUpdateChannel)
			r.With(a.managementAdmin).Put("/updates/auto-check", a.putUpdateAutoCheck)
			r.With(a.managementAdmin).Post("/updates/server", a.upgradeServerFromChannel)
			r.With(a.managementAdmin).Post("/updates/server/upload", a.upgradeServerFromUpload)
			r.With(a.managementAdmin).Get("/usage-report", a.getUsageReport)
			r.With(a.managementAdmin).Put("/usage-report/enabled", a.putUsageReportEnabled)
			r.With(a.managementAdmin).Get("/usage-report/preview", a.getUsageReportPreview)
			r.With(a.managementAdmin).Get("/diagnostics/debug-session", a.getDebugSession)
			r.With(a.managementAdmin).Post("/diagnostics/debug-session", a.postDebugSession)
			r.With(a.managementAdmin).Delete("/diagnostics/debug-session", a.deleteDebugSession)
			r.With(a.managementAdmin).Get("/diagnostics/bundles", a.getDiagnosticsBundles)
			r.With(a.managementAdmin).Post("/diagnostics/bundles", a.postDiagnosticsBundle)
			r.With(a.managementAdmin).Get("/diagnostics/bundles/{id}", a.getDiagnosticsBundle)
			r.With(a.managementAdmin).Get("/diagnostics/bundles/{id}/download", a.downloadDiagnosticsBundle)
			r.With(a.managementAdmin).Get("/experience", a.getExperience)
			r.With(a.managementAdmin).Put("/experience/enabled", a.putExperienceEnabled)
			r.With(a.managementAdmin).Post("/experience/reset", a.postExperienceReset)
			r.With(a.managementAdmin).Post("/experience/password", a.postExperiencePassword)
			r.Get("/notices", a.getNotices)
			r.Post("/notices/read", a.postNoticesRead)
			r.With(a.managementAdmin).Get("/server/outbound-policy", a.getServerOutboundPolicy)
			r.With(a.managementAdmin).Put("/server/outbound-policy", a.putServerOutboundPolicy)
			r.With(a.managementAdmin).Post("/server/outbound-policy/check", a.checkServerOutboundPolicy)
			r.With(a.managementAdmin).Get("/server/listen", a.getServerListen)
			r.With(a.managementAdmin).Put("/server/listen", a.putServerListen)
			r.With(a.managementAdmin).Post("/server/restart", a.restartServer)

			// Metrics + logs.
			r.Get("/metrics/query", a.metricsQuery)
			r.Get("/logs/search", a.logsSearch)
			r.Get("/logs/context", a.logsContext)
			r.Get("/logs/histogram", a.logsHistogram)
			r.Get("/monitor/overview", a.monitorOverview)
			r.Get("/scheduling/board", a.schedulingBoard)
			r.Get("/scheduling/overview", a.schedulingOverview)
			r.Get("/scheduling/history", a.schedulingHistory)
			r.Get("/scheduling/timeline", a.schedulingTimeline)
			r.Get("/scheduling/activity", a.schedulingActivity)
			r.Get("/scheduling/yield-policies", a.yieldPolicyDictionary)
			r.Get("/monitor/deployments/{id}", a.monitorDeployment)
			r.Get("/monitor/deployments/{id}/requests", a.monitorRequests)
			r.Get("/monitor/requests/lookup", a.monitorRequestLookup)
			r.Get("/monitor/distributions", a.monitorDistributions)
			r.Get("/monitor/deployments/{id}/latency", a.monitorLatency)
			r.Get("/monitor/metrics", a.monitorMetrics)
			r.Get("/monitor/series", a.monitorSeries)
			r.Get("/gpus", a.listGPUs)
			r.Get("/dgpus", a.listDGPUs)

			// AI helper.
			r.Get("/ai/configs", a.listAIConfigs)
			r.Post("/ai/configs", a.createAIConfig)
			// The connection test changes nothing, so it is a POST to a verb
			// path rather than a resource write under {id}.
			r.Post("/ai/configs/test", a.testAIConfig)
			r.Patch("/ai/configs/{id}", a.updateAIConfig)
			r.Delete("/ai/configs/{id}", a.deleteAIConfig)
			r.Get("/ai/available", a.aiAvailable)
			r.Post("/ai/ask", a.aiAsk)
			r.Get("/ai/instances/{uuid}/findings", a.aiInstanceFindings)
			r.Get("/ai/models/{id}/fit", a.aiModelFit)

			// Playground stays inside management JWT auth. Its proxy may attach an
			// encrypted external-provider key, so it must never be mounted at /v1.
			r.Get("/playground/models", a.playgroundModels)
			r.Get("/playground/openapi.json", func(w http.ResponseWriter, req *http.Request) {
				if d.PlaygroundV1 == nil {
					http.NotFound(w, req)
					return
				}
				d.PlaygroundV1.ServeHTTP(w, req)
			})
			if d.PlaygroundV1 != nil {
				r.Handle("/playground/v1/*", a.withClaims(d.PlaygroundV1))
			}

			// Batches: the console's side of the Batch API. Anyone signed in reads
			// (an administrator sees every user's), an administrator writes.
			r.Get("/batches", a.listBatches)
			r.With(a.managementAdmin).Post("/batches", a.createBatch)
			r.Get("/batches/dictionary", a.batchDictionary)
			r.Get("/batches/{id}", a.getBatch)
			r.With(a.managementAdmin).Post("/batches/{id}/cancel", a.cancelBatch)
			r.Get("/batches/{id}/files/{kind}", a.downloadBatchFile)
		})
	})
	r.Get("/healthz", a.health)

	// SPA fallback for everything else.
	if d.UIHandler != nil {
		r.Handle("/*", d.UIHandler)
	}
	a.mux = r
	return r
}
