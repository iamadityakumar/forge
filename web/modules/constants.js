// Forge Dashboard — Constants & Configuration
export const METRIC = {
  // Orchestrator metrics (forge_api_*)
  jobsSubmitted: 'forge_api_jobs_submitted_total',
  jobsCompleted: 'forge_api_jobs_completed_total',
  jobsFailed: 'forge_api_jobs_failed_total',
  jobsRejected: 'forge_api_jobs_rejected_total',
  pendingJobs: 'forge_api_pending_jobs',
  activeWorkers: 'forge_api_active_workers',
  rateLimitWaits: 'forge_api_rate_limit_waits_total',
  httpRequests: 'forge_api_http_requests_total',
  httpRequestDuration: 'forge_api_http_request_duration_seconds',
  jobDuration: 'forge_api_job_duration_seconds',
  claimsTotal: 'forge_api_claims_total',
  jobsCompletedApi: 'forge_api_jobs_completed_total',
  inFlightJobs: 'forge_api_in_flight_jobs',
  leaseExtensions: 'forge_api_lease_extensions_total',
  llmTokens: 'forge_api_llm_tokens_total',

  // Worker metrics (forge_worker_*) - fetched via proxy
  workerClaims: 'forge_worker_claims_total',
  workerJobsCompleted: 'forge_worker_jobs_completed_total',
  workerJobsFailed: 'forge_worker_jobs_failed_total',
  workerJobDuration: 'forge_worker_job_duration_seconds',
  workerLeaseExtensions: 'forge_worker_lease_extensions_total',
  workerInFlightJobs: 'forge_worker_in_flight_jobs',
  workerStepsTotal: 'forge_worker_steps_total',
  workerStepDuration: 'forge_worker_step_duration_seconds',
  workerStepsResumed: 'forge_worker_steps_resumed_total',
  workerLLMCalls: 'forge_worker_llm_calls_total',
  workerLLMDuration: 'forge_worker_llm_duration_seconds',
  workerLLMTokens: 'forge_worker_llm_tokens_total',
  workerLLMErrors: 'forge_worker_llm_errors_total',
  workerRateLimitWaits: 'forge_worker_rate_limit_waits_total',
  workerRateLimitWaitTime: 'forge_worker_rate_limit_wait_seconds',
};

export const SPARKLINE_HISTORY_MAX = 60; // 60 points = 5 minutes at 5s interval

export const THRESHOLDS = {
  pendingJobs: { warning: 10, critical: 50 },
  rateLimitWaits: { warning: 5, critical: 20 },
  activeWorkers: { warning: 2, critical: 1 }, // min expected workers
};

export const PAGE_SIZES = [10, 20, 50, 100];
export const DEFAULT_PAGE_SIZE = 20;
export const POLL_INTERVAL = 5000;
export const TRACE_INTERVAL = 2000;

export const JAEGER_BASE_URL = 'http://localhost:16686';