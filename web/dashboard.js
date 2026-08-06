// Forge Dashboard Engine — High-Performance Chart.js Observability
(function () {
  let selectedJobId = null;
  let selectedJobTraceContext = null;
  let pollInterval = 5000;
  let traceInterval = 2000;
  let parsedMetrics = {};
  let workerList = [];
  let workerMetricsCache = {}; // workerName -> parsed metrics

  // Pagination state
  let jobsCurrentPage = 1;
  let jobsPageSize = 20;
  let jobsTotalCount = 0;

  // Metric name constants with correct namespaces
  const METRIC = {
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

  // Sparkline configuration
  const SPARKLINE_HISTORY_MAX = 60; // 60 points = 5 minutes at 5s interval
  const THRESHOLDS = {
    pendingJobs: { warning: 10, critical: 50 },
    rateLimitWaits: { warning: 5, critical: 20 },
    activeWorkers: { warning: 2, critical: 1 }, // min expected workers
  };

  // Sparkline history buffers (circular)
  const metricHistory = {};
  function addToHistory(metricName, value) {
    if (!metricHistory[metricName]) {
      metricHistory[metricName] = [];
    }
    metricHistory[metricName].push({ time: Date.now(), value });
    if (metricHistory[metricName].length > SPARKLINE_HISTORY_MAX) {
      metricHistory[metricName].shift();
    }
  }
  function getHistory(metricName) {
    return metricHistory[metricName] || [];
  }

  // Chart.js instance references
  let chartJobStatus = null;
  let chartStepDuration = null;
  let chartTokens = null;
  let sparklineCharts = {}; // statName -> Chart instance

  // Initialize
  document.addEventListener('DOMContentLoaded', () => {
    fetchHealth();
    fetchMetrics();
    fetchWorkers();
    fetchJobs();

    setInterval(fetchHealth, pollInterval);
    setInterval(fetchMetrics, pollInterval);
    setInterval(fetchWorkers, pollInterval);
    setInterval(fetchJobs, pollInterval);
    setInterval(fetchSelectedTrace, traceInterval);
  });

  // Prometheus Metrics Parser
  function parsePrometheusText(text) {
    const metrics = {};
    const lines = text.split('\n');

    for (let line of lines) {
      line = line.trim();
      if (!line || line.startsWith('#')) continue;

      let nameAndLabels = line;
      let valueStr = '';
      const spaceIdx = line.lastIndexOf(' ');
      if (spaceIdx !== -1) {
        nameAndLabels = line.substring(0, spaceIdx).trim();
        valueStr = line.substring(spaceIdx + 1).trim();
      }

      const val = parseFloat(valueStr);
      if (isNaN(val)) continue;

      let name = nameAndLabels;
      let labels = {};

      const braceIdx = nameAndLabels.indexOf('{');
      if (braceIdx !== -1) {
        name = nameAndLabels.substring(0, braceIdx);
        const labelStr = nameAndLabels.substring(braceIdx + 1, nameAndLabels.lastIndexOf('}'));
        const labelPairs = labelStr.split(',');
        for (let pair of labelPairs) {
          const eqIdx = pair.indexOf('=');
          if (eqIdx !== -1) {
            const k = pair.substring(0, eqIdx).trim();
            const v = pair.substring(eqIdx + 1).replace(/^"|"$/g, '').trim();
            labels[k] = v;
          }
        }
      }

      if (!metrics[name]) {
        metrics[name] = [];
      }
      metrics[name].push({ labels, value: val });
    }
    return metrics;
  }

  // Merge worker metrics into parsedMetrics
  function mergeWorkerMetrics() {
    for (let workerName in workerMetricsCache) {
      const wMetrics = workerMetricsCache[workerName];
      for (let name in wMetrics) {
        if (!parsedMetrics[name]) {
          parsedMetrics[name] = [];
        }
        // Prefix worker metrics with worker name for disambiguation if needed
        // But we keep the original names so that the same metric from different workers can be summed
        parsedMetrics[name].push(...wMetrics[name]);
      }
    }
  }

  // Fetch API Metrics (orchestrator + workers)
  async function fetchMetrics() {
    try {
      // Fetch orchestrator metrics
      const res = await fetch('/metrics');
      if (res.ok) {
        const text = await res.text();
        parsedMetrics = parsePrometheusText(text);
      }

      // Fetch worker metrics in parallel
      await fetchAllWorkerMetrics();

      // Merge worker metrics into parsedMetrics
      mergeWorkerMetrics();

      updateStats();
      renderCharts();
    } catch (err) {
      console.error('Failed to fetch metrics:', err);
    }
  }

  // Fetch all worker metrics in parallel
  async function fetchAllWorkerMetrics() {
    if (workerList.length === 0) return;

    const promises = workerList.map(w => fetchWorkerMetricsRaw(w));
    await Promise.all(promises);
  }

  // Fetch raw worker metrics (for merging into parsedMetrics)
  async function fetchWorkerMetricsRaw(workerName) {
    try {
      const res = await fetch(`/api/worker-metrics/${encodeURIComponent(workerName)}`);
      if (!res.ok) throw new Error('Offline');
      const text = await res.text();
      workerMetricsCache[workerName] = parsePrometheusText(text);
    } catch (err) {
      workerMetricsCache[workerName] = {};
    }
  }

  function getMetricVal(name, matchLabels = {}) {
    const list = parsedMetrics[name];
    if (!list) return 0;
    for (let item of list) {
      let match = true;
      for (let k in matchLabels) {
        if (item.labels[k] !== matchLabels[k]) {
          match = false;
          break;
        }
      }
      if (match) return item.value;
    }
    return 0;
  }

  function sumMetricVal(name) {
    const list = parsedMetrics[name];
    if (!list) return 0;
    return list.reduce((acc, curr) => acc + curr.value, 0);
  }

  function sumMetricValWithLabel(name, matchLabels) {
    const list = parsedMetrics[name];
    if (!list) return 0;
    let sum = 0;
    for (let item of list) {
      let match = true;
      for (let k in matchLabels) {
        if (item.labels[k] !== matchLabels[k]) {
          match = false;
          break;
        }
      }
      if (match) sum += item.value;
    }
    return sum;
  }

  // Get metric value trying both orchestrator and worker namespaces
  function getMetricValAny(orchestratorName, workerName, matchLabels = {}) {
    // Try orchestrator namespace first
    let val = getMetricVal(orchestratorName, matchLabels);
    if (val > 0) return val;
    // Fall back to worker namespace (summed across workers)
    return sumMetricVal(workerName);
  }

  function sumMetricValAny(orchestratorName, workerName) {
    // Try orchestrator namespace first
    let val = sumMetricVal(orchestratorName);
    if (val > 0) return val;
    // Fall back to worker namespace
    return sumMetricVal(workerName);
  }

  function sumMetricValWithLabelAny(orchestratorName, workerName, matchLabels) {
    // Try orchestrator namespace first
    let val = sumMetricValWithLabel(orchestratorName, matchLabels);
    if (val > 0) return val;
    // Fall back to worker namespace
    return sumMetricValWithLabel(workerName, matchLabels);
  }

  // Threshold indicator logic
  function updateThresholdIndicator(element, metricName, value) {
    const thresholds = THRESHOLDS[metricName];
    if (!thresholds) return;

    element.classList.remove('threshold-warning', 'threshold-critical');

    if (metricName === 'activeWorkers') {
      // For active workers, lower is worse
      if (value <= thresholds.critical) {
        element.classList.add('threshold-critical');
      } else if (value <= thresholds.warning) {
        element.classList.add('threshold-warning');
      }
    } else {
      // For other metrics, higher is worse
      if (value >= thresholds.critical) {
        element.classList.add('threshold-critical');
      } else if (value >= thresholds.warning) {
        element.classList.add('threshold-warning');
      }
    }
  }

  // Sparkline rendering
  function renderSparklines() {
    const sparklineConfig = [
      { canvasId: 'sparklineSubmitted', metric: METRIC.jobsSubmitted, fallback: METRIC.workerJobsCompleted, color: '#10b981' },
      { canvasId: 'sparklineCompleted', metric: METRIC.jobsCompleted, fallback: METRIC.workerJobsCompleted, color: '#10b981' },
      { canvasId: 'sparklineFailed', metric: METRIC.jobsFailed, fallback: METRIC.workerJobsFailed, color: '#ef4444' },
      { canvasId: 'sparklinePending', metric: METRIC.pendingJobs, fallback: METRIC.workerInFlightJobs, color: '#f59e0b' },
      { canvasId: 'sparklineWorkers', metric: METRIC.activeWorkers, fallback: METRIC.workerClaims, color: '#6366f1' },
      { canvasId: 'sparklineWaits', metric: METRIC.rateLimitWaits, fallback: METRIC.workerRateLimitWaits, color: '#8b5cf6' },
    ];

    sparklineConfig.forEach(({ canvasId, metric, fallback, color }) => {
      const canvas = document.getElementById(canvasId);
      if (!canvas) return;

      const history = getHistory(metric);
      if (history.length === 0) {
        // Try fallback
        const fallbackHistory = getHistory(fallback);
        if (fallbackHistory.length > 0) {
          renderSparkline(canvas, fallbackHistory, color);
        }
        return;
      }
      renderSparkline(canvas, history, color);
    });
  }

  function renderSparkline(canvas, history, color) {
    const ctx = canvas.getContext('2d');
    const width = canvas.width;
    const height = canvas.height;
    const padding = 2;

    ctx.clearRect(0, 0, width, height);

    if (history.length < 2) return;

    // Get min/max for scaling
    const values = history.map(h => h.value);
    const min = Math.min(...values);
    const max = Math.max(...values);
    const range = max - min || 1;

    ctx.beginPath();
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.5;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';

    history.forEach((point, i) => {
      const x = padding + (i / (history.length - 1)) * (width - 2 * padding);
      const y = height - padding - ((point.value - min) / (max - min || 1)) * (height - 2 * padding);

      if (i === 0) {
        ctx.moveTo(x, y);
      } else {
        ctx.lineTo(x, y);
      }
    });

    ctx.stroke();

    // Draw current value dot
    const last = history[history.length - 1];
    const x = padding + ((history.length - 1) / (history.length - 1)) * (width - 2 * padding);
    const y = height - padding - ((last.value - min) / (max - min || 1)) * (height - 2 * padding);
    ctx.beginPath();
    ctx.arc(x, y, 3, 0, Math.PI * 2);
    ctx.fillStyle = color;
    ctx.fill();
  }

  function updateStats() {
    // Update stat values and history
    const statConfig = [
      { id: 'statSubmitted', metric: METRIC.jobsSubmitted, fallback: METRIC.workerJobsCompleted, addToHistory: true },
      { id: 'statCompleted', metric: METRIC.jobsCompleted, fallback: METRIC.workerJobsCompleted, addToHistory: true },
      { id: 'statFailed', metric: METRIC.jobsFailed, fallback: METRIC.workerJobsFailed, addToHistory: true },
      { id: 'statPending', metric: METRIC.pendingJobs, fallback: METRIC.workerInFlightJobs, addToHistory: true },
      { id: 'statWorkers', metric: METRIC.activeWorkers, fallback: METRIC.workerClaims, addToHistory: true },
      { id: 'statWaits', metric: METRIC.rateLimitWaits, fallback: METRIC.workerRateLimitWaits, addToHistory: true },
    ];

    statConfig.forEach(({ id, metric, fallback, addToHistory }) => {
      const val = sumMetricValAny(metric, fallback);
      const el = document.getElementById(id);
      if (el) {
        el.textContent = val;

        // Add to history for sparklines
        if (addToHistory) {
          addToHistory(metric, val);
        }

        // Update threshold indicators
        updateThresholdIndicator(el, metric, val);
      }
    });

    // Render sparklines for stats that have history
    renderSparklines();
  }

  // Fetch Workers & Worker Metrics Proxy
  async function fetchWorkers() {
    try {
      const res = await fetch('/api/workers');
      if (!res.ok) return;
      const data = await res.json();
      workerList = data.workers || [];

      const grid = document.getElementById('workersGrid');
      if (workerList.length === 0) {
        grid.innerHTML = '<div class="empty-state">No workers configured</div>';
        return;
      }

      grid.innerHTML = '';
      for (let w of workerList) {
        const card = document.createElement('div');
        card.className = 'worker-card';
        card.innerHTML = `
          <div class="worker-card-header">
            <span class="worker-name">${escapeHtml(w)}</span>
            <span class="worker-badge badge-online" id="badge-${escapeHtml(w)}">Checking...</span>
          </div>
          <div class="worker-stats">
            <span>Claims: <strong id="claims-${escapeHtml(w)}">-</strong></span>
            <span>Completed: <strong id="comp-${escapeHtml(w)}">-</strong></span>
          </div>
        `;
        grid.appendChild(card);

        // Fetch per-worker proxied metrics
        fetchWorkerMetrics(w);
      }
    } catch (err) {
      console.error('Failed to list workers:', err);
    }
  }

  async function fetchWorkerMetrics(workerName) {
    const badge = document.getElementById(`badge-${workerName}`);
    const claimsEl = document.getElementById(`claims-${workerName}`);
    const compEl = document.getElementById(`comp-${workerName}`);

    try {
      const res = await fetch(`/api/worker-metrics/${encodeURIComponent(workerName)}`);
      if (!res.ok) throw new Error('Offline');
      const text = await res.text();
      const wMetrics = parsePrometheusText(text);

      if (badge) {
        badge.className = 'worker-badge badge-online';
        badge.textContent = 'ONLINE';
      }

      // Worker metrics use forge_worker_* namespace
      const claims = wMetrics['forge_worker_claims_total'] ? wMetrics['forge_worker_claims_total'][0].value : 0;
      const comp = wMetrics['forge_worker_jobs_completed_total'] ? wMetrics['forge_worker_jobs_completed_total'][0].value : 0;

      if (claimsEl) claimsEl.textContent = claims;
      if (compEl) compEl.textContent = comp;
    } catch (err) {
      if (badge) {
        badge.className = 'worker-badge badge-offline';
        badge.textContent = 'OFFLINE';
      }
    }
  }

  // Fetch Jobs List with pagination
  async function fetchJobs() {
    try {
      const offset = (jobsCurrentPage - 1) * jobsPageSize;
      const res = await fetch(`/jobs?limit=${jobsPageSize}&offset=${offset}`);
      if (!res.ok) return;
      const jobs = await res.json();

      // Get total count from a separate request if not cached
      if (jobsTotalCount === 0) {
        try {
          const countRes = await fetch('/jobs?limit=1');
          const allJobs = await countRes.json();
          jobsTotalCount = allJobs.length; // This is just for first page, we'd need a count endpoint for accurate total
          // For now, we'll estimate from first page
        } catch (e) {
          console.warn('Could not fetch total count');
        }
      }

      const tbody = document.getElementById('jobsTableBody');
      if (!jobs || jobs.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" class="empty-state">No jobs found</td></tr>';
        renderPagination();
        return;
      }

      tbody.innerHTML = '';
      for (let j of jobs) {
        const tr = document.createElement('tr');
        tr.className = `job-row ${selectedJobId === j.id ? 'selected' : ''}`;
        tr.onclick = () => selectJob(j.id);

        const statusClass = `badge-${j.status}`;

        tr.innerHTML = `
          <td class="code-pill">${j.id.substring(0, 8)}...</td>
          <td><strong>${escapeHtml(j.task_type)}</strong></td>
          <td><span class="badge ${statusClass}">${escapeHtml(j.status)}</span></td>
          <td>${j.attempt_count} / ${j.max_attempts}</td>
          <td>${j.claimed_by ? `<span class="worker-tag">${escapeHtml(j.claimed_by)}</span>` : '<span style="color:var(--text-dim)">-</span>'}</td>
          <td style="color:var(--text-muted); font-size:12px;">${new Date(j.created_at).toLocaleTimeString()}</td>
        `;
        tbody.appendChild(tr);
      }
      renderPagination();
    } catch (err) {
      console.error('Failed to fetch jobs:', err);
    renderPagination();
    } catch (err) {
      console.error('Failed to fetch jobs:', err);
    }
  }

  // Pagination rendering
  function renderPagination() {
    const container = document.getElementById('jobsPagination');
    if (!container) return;

    const totalPages = Math.ceil(jobsTotalCount / jobsPageSize) || 1;
    if (totalPages <= 1) {
      container.innerHTML = '';
      return;
    }

    let html = '<div class="pagination">';

    // Previous button
    if (jobsCurrentPage > 1) {
      html += `<button class="page-btn" data-page="${jobsCurrentPage - 1}" aria-label="Previous">‹ Prev</button>`;
    }

    // Page numbers
    const maxPages = 5;
    let startPage = Math.max(1, jobsCurrentPage - Math.floor(maxPages / 2));
    let endPage = Math.min(totalPages, startPage + maxPages - 1);

    if (endPage - startPage + 1 < maxPages) {
      startPage = Math.max(1, endPage - maxPages + 1);
    }

    for (let i = startPage; i <= endPage; i++) {
      if (i === jobsCurrentPage) {
        html += `<button class="page-btn current" aria-current="page">${i}</button>`;
      } else {
        html += `<button class="page-btn" data-page="${i}">${i}</button>`;
      }
    }

    // Next button
    if (jobsCurrentPage < totalPages) {
      html += `<button class="page-btn" data-page="${jobsCurrentPage + 1}" aria-label="Next">Next ›</button>`;
    }

    // Page size selector
    html += `
      <select id="pageSizeSelect" class="page-size-select" aria-label="Page size">
        <option value="10" ${jobsPageSize === 10 ? 'selected' : ''}>10 per page</option>
        <option value="20" ${jobsPageSize === 20 ? 'selected' : ''}>20 per page</option>
        <option value="50" ${jobsPageSize === 50 ? 'selected' : ''}>50 per page</option>
        <option value="100" ${jobsPageSize === 100 ? 'selected' : ''}>100 per page</option>
      </select>
    `;

    html += '</div>';
    container.innerHTML = html;

    // Add event listeners
    container.querySelectorAll('.page-btn[data-page]').forEach(btn => {
      btn.addEventListener('click', () => {
        jobsCurrentPage = parseInt(btn.dataset.page, 10);
        fetchJobs();
      });
    });

    const pageSizeSelect = document.getElementById('pageSizeSelect');
    if (pageSizeSelect) {
      pageSizeSelect.addEventListener('change', (e) => {
        jobsPageSize = parseInt(e.target.value, 10);
        jobsCurrentPage = 1; // Reset to first page
        fetchJobs();
      });
    }

    function selectJob(id) {
    try {
      const res = await fetch(`/jobs/${id}`);
      if (res.ok) {
        const job = await res.json();
        selectedJobTraceContext = job.trace_context || null;
        updateJaegerLink();
      }
    } catch (err) {
      console.error('Failed to fetch job details:', err);
    }
  }

  function updateJaegerLink() {
    const linkEl = document.getElementById('jaegerLink');
    if (!linkEl) return;
    if (selectedJobTraceContext && selectedJobTraceContext.trace_id) {
      const traceId = selectedJobTraceContext.trace_id;
      const jaegerUrl = `http://localhost:16686/trace/${traceId}`;
      linkEl.href = jaegerUrl;
      linkEl.style.display = 'inline-flex';
      linkEl.textContent = `🔗 View in Jaeger (${traceId.substring(0, 16)}...)`;
    } else {
      linkEl.style.display = 'none';
    }
  }

  async function fetchSelectedTrace() {
    if (!selectedJobId) return;
    try {
      const [traceRes, llmRes] = await Promise.all([
        fetch(`/jobs/${selectedJobId}/trace`),
        fetch(`/jobs/${selectedJobId}/llm_calls`)
      ]);

      if (!traceRes.ok) return;
      const steps = await traceRes.json();

      const timeline = document.getElementById('stepTimeline');
      const reclaimNotice = document.getElementById('reclaimNotice');
      const llmContainer = document.getElementById('llmCallsContainer');

      if (!steps || steps.length === 0) {
        timeline.innerHTML = '<div class="empty-state">No steps recorded yet for this job</div>';
        if (reclaimNotice) reclaimNotice.style.display = 'none';
        if (llmContainer) llmContainer.innerHTML = '<div class="empty-state">No LLM calls recorded</div>';
        return;
      }

      const workersSeen = new Set();
      timeline.innerHTML = '';

      for (let s of steps) {
        if (s.worker_id) workersSeen.add(s.worker_id);

        const item = document.createElement('div');
        item.className = 'timeline-step';

        let outputStr = '';
        if (s.output) {
          try {
            outputStr = typeof s.output === 'string' ? s.output : JSON.stringify(s.output, null, 2);
          } catch (e) {
            outputStr = String(s.output);
          }
        }

        item.innerHTML = `
          <div class="step-header">
            <div>
              <span class="step-type">Step ${s.step_number}: ${escapeHtml(s.step_type)}</span>
              <span style="margin-left: 8px; font-size: 11px; color: var(--text-dim);">${s.duration_ms}ms</span>
            </div>
            ${s.worker_id ? `<span class="worker-tag">${escapeHtml(s.worker_id)}</span>` : ''}
          </div>
          ${outputStr ? `<pre class="step-output">${escapeHtml(outputStr)}</pre>` : ''}
        `;
        timeline.appendChild(item);
      }

      if (reclaimNotice) {
        reclaimNotice.style.display = workersSeen.size > 1 ? 'block' : 'none';
      }

      // Render LLM calls
      if (llmContainer) {
        if (!llmRes.ok) {
          llmContainer.innerHTML = '<div class="empty-state">Failed to fetch LLM calls</div>';
          llmContainer.style.display = 'none';
        } else {
          const llmCalls = await llmRes.json();
          if (llmCalls && llmCalls.length > 0) {
            renderLLMCalls(llmCalls, llmContainer);
            llmContainer.style.display = 'block';
          } else {
            llmContainer.innerHTML = '<div class="empty-state">No LLM calls recorded for this job</div>';
            llmContainer.style.display = 'none';
          }
        }
      }
    } catch (err) {
      console.error('Failed to fetch trace:', err);
    }
  }

  function renderLLMCalls(calls, container) {
    if (!calls || calls.length === 0) {
      container.innerHTML = '<div class="empty-state">No LLM calls recorded for this job</div>';
      return;
    }

    let html = '<div class="llm-calls-header"><strong>LLM Calls</strong></div>';
    html += '<div class="llm-calls-list">';

    for (let call of calls) {
      const duration = call.latency_ms ? `${call.latency_ms}ms` : 'N/A';
      const tokens = (call.prompt_tokens || 0) + (call.completion_tokens || 0);
      const statusClass = call.error ? 'badge-failed' : 'badge-completed';
      const statusText = call.error ? 'Failed' : 'Completed';

      html += `
        <div class="llm-call-item">
          <div class="llm-call-header">
            <span class="llm-call-backend">${escapeHtml(call.backend || 'unknown')}</span>
            <span class="badge ${statusClass}">${statusText}</span>
          </div>
          <div class="llm-call-details">
            <span>Duration: ${duration}</span>
            <span>Tokens: ${tokens} (prompt: ${call.prompt_tokens || 0}, completion: ${call.completion_tokens || 0})</span>
            ${call.error ? `<span class="llm-error">${escapeHtml(call.error)}</span>` : ''}
          </div>
          <div class="llm-call-time" style="font-size: 11px; color: var(--text-dim);">
            ${call.created_at ? new Date(call.created_at).toLocaleString() : 'Unknown time'}
          </div>
        </div>
      `;
    }

    html += '</div>';
    container.innerHTML = html;
  }

  // Chart.js Rendering Engine
  function renderCharts() {
    if (typeof Chart === 'undefined') {
      console.warn('Chart.js library not loaded');
      return;
    }

    renderStatusChart();
    renderStepDurationChart();
    renderTokensChart();
  }

  function renderStatusChart() {
    const canvas = document.getElementById('chartJobStatus');
    if (!canvas) return;

    // Use orchestrator metrics for job status, fall back to worker metrics
    const completed = sumMetricValAny(METRIC.jobsCompleted, METRIC.workerJobsCompleted);
    const failed = sumMetricValAny(METRIC.jobsFailed, METRIC.workerJobsFailed);
    const pending = getMetricValAny(METRIC.pendingJobs, METRIC.workerInFlightJobs);

    const labels = ['Completed', 'Pending', 'Failed'];
    const dataValues = [completed, pending, failed];

    if (chartJobStatus) {
      chartJobStatus.data.datasets[0].data = dataValues;
      chartJobStatus.update();
    } else {
      chartJobStatus = new Chart(canvas, {
        type: 'doughnut',
        data: {
          labels: labels,
          datasets: [{
            data: dataValues,
            backgroundColor: ['#10b981', '#f59e0b', '#ef4444'],
            borderColor: '#111827',
            borderWidth: 2
          }]
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          animation: { duration: 300 },
          plugins: {
            legend: {
              position: 'bottom',
              labels: { color: '#9ca3af', font: { size: 11 } }
            }
          }
        }
      });
    }
  }

  function renderStepDurationChart() {
    const canvas = document.getElementById('chartStepDuration');
    if (!canvas) return;

    // Step duration metrics are in forge_worker_* namespace (from worker metrics)
    const histogram = parsedMetrics['forge_worker_step_duration_seconds_bucket'] || [];
    const labels = [];
    const cumulativeValues = [];

    // Extract cumulative bucket values (sorted by le)
    const sortedBuckets = histogram
      .filter(item => item.labels.le)
      .sort((a, b) => {
        const leA = a.labels.le === '+Inf' ? Infinity : parseFloat(a.labels.le);
        const leB = b.labels.le === '+Inf' ? Infinity : parseFloat(b.labels.le);
        return leA - leB;
      });

    for (let item of sortedBuckets) {
      const lbl = item.labels.le === '+Inf' ? '+Inf' : `≤${item.labels.le}s`;
      labels.push(lbl);
      cumulativeValues.push(item.value);
    }

    // Convert cumulative to per-bucket counts
    const dataValues = [];
    let prev = 0;
    for (let val of cumulativeValues) {
      dataValues.push(Math.max(0, val - prev));
      prev = val;
    }

    if (labels.length === 0) {
      labels.push('No steps');
      dataValues.push(0);
    }

    if (chartStepDuration) {
      chartStepDuration.data.labels = labels;
      chartStepDuration.data.datasets[0].data = dataValues;
      chartStepDuration.update();
    } else {
      chartStepDuration = new Chart(canvas, {
        type: 'bar',
        data: {
          labels: labels,
          datasets: [{
            label: 'Step Count',
            data: dataValues,
            backgroundColor: '#6366f1',
            borderRadius: 4
          }]
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          animation: { duration: 300 },
          plugins: {
            legend: { display: false }
          },
          scales: {
            x: {
              ticks: { color: '#9ca3af', font: { size: 10 } },
              grid: { color: '#1f2d42' }
            },
            y: {
              beginAtZero: true,
              ticks: { color: '#9ca3af', font: { size: 10 } },
              grid: { color: '#1f2d42' }
            }
          }
        }
      });
    }
  }

  function renderTokensChart() {
    const canvas = document.getElementById('chartTokens');
    if (!canvas) return;

    // Get all token series grouped by backend and kind (forge_worker_* namespace)
    const tokenMetrics = parsedMetrics['forge_worker_llm_tokens_total'] || [];
    const backendMap = new Map(); // backend -> {prompt: val, completion: val}

    for (let item of tokenMetrics) {
      const backend = item.labels.backend || 'unknown';
      const kind = item.labels.kind || 'unknown';
      if (!backendMap.has(backend)) {
        backendMap.set(backend, { prompt: 0, completion: 0 });
      }
      const entry = backendMap.get(backend);
      if (kind === 'prompt') entry.prompt = item.value;
      else if (kind === 'completion') entry.completion = item.value;
    }

    const backends = Array.from(backendMap.keys());
    if (backends.length === 0) {
      // Fallback: try orchestrator namespace, then worker namespace
      const promptTokens = sumMetricValWithLabelAny(METRIC.LLMTokens, METRIC.workerLLMTokens, { kind: 'prompt' });
      const compTokens = sumMetricValWithLabelAny(METRIC.LLMTokens, METRIC.workerLLMTokens, { kind: 'completion' });
      renderSingleBackendChart(canvas, promptTokens, compTokens);
      return;
    }

    const promptData = backends.map(b => backendMap.get(b).prompt);
    const compData = backends.map(b => backendMap.get(b).completion);

    if (chartTokens) {
      chartTokens.data.labels = backends;
      chartTokens.data.datasets[0].data = promptData;
      chartTokens.data.datasets[1].data = compData;
      chartTokens.update();
    } else {
      chartTokens = new Chart(canvas, {
        type: 'bar',
        data: {
          labels: backends,
          datasets: [
            {
              label: 'Prompt Tokens',
              data: promptData,
              backgroundColor: '#3b82f6',
              borderRadius: 4
            },
            {
              label: 'Completion Tokens',
              data: compData,
              backgroundColor: '#8b5cf6',
              borderRadius: 4
            }
          ]
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          animation: { duration: 300 },
          plugins: {
            legend: {
              position: 'bottom',
              labels: { color: '#9ca3af', font: { size: 11 } }
            }
          },
          scales: {
            x: {
              stacked: false,
              ticks: { color: '#9ca3af', font: { size: 11 } },
              grid: { color: '#1f2d42' }
            },
            y: {
              stacked: false,
              beginAtZero: true,
              ticks: { color: '#9ca3af', font: { size: 11 } },
              grid: { color: '#1f2d42' }
            }
          }
        }
      });
    }
  }

  function renderSingleBackendChart(canvas, promptTokens, compTokens) {
    const labels = ['Tokens'];
    if (chartTokens) {
      chartTokens.data.labels = labels;
      chartTokens.data.datasets[0].data = [promptTokens];
      chartTokens.data.datasets[1].data = [compTokens];
      chartTokens.update();
    } else {
      chartTokens = new Chart(canvas, {
        type: 'bar',
        data: {
          labels: labels,
          datasets: [
            {
              label: 'Prompt Tokens',
              data: [promptTokens],
              backgroundColor: '#3b82f6',
              borderRadius: 4
            },
            {
              label: 'Completion Tokens',
              data: [compTokens],
              backgroundColor: '#8b5cf6',
              borderRadius: 4
            }
          ]
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          animation: { duration: 300 },
          plugins: {
            legend: {
              position: 'bottom',
              labels: { color: '#9ca3af', font: { size: 11 } }
            }
          },
          scales: {
            x: {
              stacked: true,
              ticks: { color: '#9ca3af', font: { size: 11 } },
              grid: { color: '#1f2d42' }
            },
            y: {
              stacked: true,
              beginAtZero: true,
              ticks: { color: '#9ca3af', font: { size: 11 } },
              grid: { color: '#1f2d42' }
            }
          }
        }
      });
    }
  }

  // Utilities
  function formatUptime(sec) {
    const h = Math.floor(sec / 3600);
    const m = Math.floor((sec % 3600) / 60);
    const s = sec % 60;
    if (h > 0) return `${h}h ${m}m`;
    if (m > 0) return `${m}m ${s}s`;
    return `${s}s`;
  }

  function escapeHtml(str) {
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }
})();