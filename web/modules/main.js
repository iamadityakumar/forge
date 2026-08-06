// Forge Dashboard — Main Entry Point
import { METRIC, POLL_INTERVAL, TRACE_INTERVAL } from './constants.js';
import { parsePrometheusText } from './metricsParser.js';
import { addToHistory, getHistory } from './history.js';
import { renderCharts } from './charts.js';
import { updateStats } from './stats.js';
import { fetchWorkers, fetchWorkerMetrics, fetchWorkerMetricsRaw, fetchAllWorkerMetrics, getWorkerMetricsCache } from './workers.js';
import { fetchJobs, renderPagination, getPaginationState, setPaginationState } from './jobs.js';
import { fetchSelectedTrace, fetchJobDetails, updateJaegerLink, renderLLMCalls } from './trace.js';
import { fetchHealth } from './health.js';
import { formatUptime, escapeHtml } from './utils.js';

// Global state
let selectedJobId = null;
let selectedJobTraceContext = null;
let parsedMetrics = {};
let workerList = [];
let workerMetricsCache = {};
let jobsCurrentPage = 1;
let jobsPageSize = 20;
let jobsTotalCount = 0;

// Chart.js instance references
let chartJobStatus = null;
let chartStepDuration = null;
let chartTokens = null;
let sparklineCharts = {};

// Make functions globally available for inline event handlers
window.escapeHtml = escapeHtml;
window.selectJob = selectJob;

// Initialize
document.addEventListener('DOMContentLoaded', async () => {
  await fetchHealth();
  await fetchMetrics();
  await fetchWorkers();
  await fetchJobs();

  setInterval(fetchHealth, 5000);
  setInterval(fetchMetrics, 5000);
  setInterval(fetchWorkers, 5000);
  setInterval(fetchJobs, 5000);
  setInterval(fetchSelectedTrace, 2000);
});

// Metrics fetching
async function fetchMetrics() {
  try {
    const res = await fetch('/metrics');
    if (res.ok) {
      const text = await res.text();
      parsedMetrics = parsePrometheusText(text);
    }

    await fetchAllWorkerMetrics();
    mergeWorkerMetrics();

    updateStats(parsedMetrics);
    renderCharts(parsedMetrics);
  } catch (err) {
    console.error('Failed to fetch metrics:', err);
  }
}

function mergeWorkerMetrics() {
  const cache = getWorkerMetricsCache();
  for (let workerName in cache) {
    const wMetrics = cache[workerName];
    for (let name in wMetrics) {
      if (!parsedMetrics[name]) {
        parsedMetrics[name] = [];
      }
      parsedMetrics[name].push(...wMetrics[name]);
    }
  }
}

async function fetchAllWorkerMetrics() {
  const { fetchWorkerMetricsRaw } = await import('./workers.js');
  if (!workerList || workerList.length === 0) return;

  const promises = workerList.map(w => fetchWorkerMetricsRaw(w));
  const results = await Promise.all(promises);
  results.forEach((metrics, i) => {
    workerMetricsCache[workerList[i]] = metrics;
  });
}

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

async function fetchWorkerMetricsRaw(workerName) {
  try {
    const res = await fetch(`/api/worker-metrics/${encodeURIComponent(workerName)}`);
    if (!res.ok) throw new Error('Offline');
    const text = await res.text();
    return parsePrometheusText(text);
  } catch (err) {
    return {};
  }
}

async function fetchAllWorkerMetrics() {
  if (!workerList || workerList.length === 0) return;

  const promises = workerList.map(w => fetchWorkerMetricsRaw(w));
  const results = await Promise.all(promises);
  results.forEach((metrics, i) => {
    workerMetricsCache[workerList[i]] = metrics;
  });
}

async function fetchJobs() {
  try {
    const offset = (window.jobsCurrentPage - 1) * window.jobsPageSize;
    const res = await fetch(`/jobs?limit=${window.jobsPageSize}&offset=${offset}`);
    if (!res.ok) return;
    const jobs = await res.json();

    const tbody = document.getElementById('jobsTableBody');
    if (!jobs || jobs.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty-state">No jobs found</td></tr>';
      renderPagination();
      return;
    }

    tbody.innerHTML = '';
    for (let j of jobs) {
      const tr = document.createElement('tr');
      tr.className = `job-row ${window.selectedJobId === j.id ? 'selected' : ''}`;
      tr.onclick = () => selectJob(j.id);

      const statusClass = `badge-${j.status}`;

      tr.innerHTML = `
        <td class="code-pill">${j.id.substring(0, 8)}...</td>
        <td><strong>${escapeHtml(j.task_type)}</strong></td>
        <td><span class="badge ${statusClass}">${escapeHtml(j.status)}</span></td>
        <td>${j.attempt_count} / ${j.max_attempts}</td>
        <td>${j.claimed_by ? `<span class="worker-tag">${escapeHtml(j.claimed_by)}</span>` : '<span style="color:var(--text-dim)">-</span>'}</td>
        <td style="color:var(--text-dim); font-size:12px;">${new Date(j.created_at).toLocaleTimeString()}</td>
      `;
      tbody.appendChild(tr);
    }
    renderPagination();
  } catch (err) {
    console.error('Failed to fetch jobs:', err);
  }
}

function selectJob(id) {
  window.selectedJobId = id;
  document.getElementById('selectedJobId').textContent = id;
  document.getElementById('timelineContainer').style.display = 'block';
  fetchSelectedTrace();
  fetchJobDetails(id);
  fetchJobs();
}

async function fetchJobDetails(id) {
  try {
    const res = await fetch(`/jobs/${id}`);
    if (res.ok) {
      const job = await res.json();
      window.selectedJobTraceContext = job.trace_context || null;
      updateJaegerLink();
    }
  } catch (err) {
    console.error('Failed to fetch job details:', err);
  }
}

function updateJaegerLink() {
  const linkEl = document.getElementById('jaegerLink');
  if (!linkEl) return;
  if (window.selectedJobTraceContext && window.selectedJobTraceContext.trace_id) {
    const traceId = window.selectedJobTraceContext.trace_id;
    const jaegerUrl = `http://localhost:16686/trace/${traceId}`;
    linkEl.href = jaegerUrl;
    linkEl.style.display = 'inline-flex';
    linkEl.textContent = `🔗 View in Jaeger (${traceId.substring(0, 16)}...)`;
  } else {
    linkEl.style.display = 'none';
  }
}

async function fetchSelectedTrace() {
  const id = window.selectedJobId;
  if (!id) return;
  try {
    const [traceRes, llmRes] = await Promise.all([
      fetch(`/jobs/${id}/trace`),
      fetch(`/jobs/${id}/llm_calls`)
    ]);

    if (!traceRes.ok) return;
    const steps = await traceRes.json();

    const timeline = document.getElementById('stepTimeline');
    const reclaimNotice = document.getElementById('reclaimNotice');
    const llmContainer = document.getElementById('llmCallsContainer');

    if (!steps || steps.length === 0) {
      timeline.innerHTML = '<div class="empty-state">No steps recorded yet for this job</div>';
      if (reclaimNotice) reclaimNotice.style.display = 'none';
      if (llmContainer) {
        llmContainer.innerHTML = '<div class="empty-state">No LLM calls recorded</div>';
        llmContainer.style.display = 'none';
      }
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

  function renderPagination() {
    const container = document.getElementById('jobsPagination');
    if (!container) return;

    const totalPages = Math.ceil(window.jobsTotalCount / window.jobsPageSize) || 1;
    if (totalPages <= 1) {
      container.innerHTML = '';
      return;
    }

    let html = '<div class="pagination">';

    if (window.jobsCurrentPage > 1) {
      html += `<button class="page-btn" data-page="${window.jobsCurrentPage - 1}" aria-label="Previous">‹ Prev</button>`;
    }

    const maxPages = 5;
    let startPage = Math.max(1, window.jobsCurrentPage - Math.floor(maxPages / 2));
    let endPage = Math.min(totalPages, startPage + maxPages - 1);

    if (endPage - startPage + 1 < maxPages) {
      startPage = Math.max(1, endPage - maxPages + 1);
    }

    for (let i = startPage; i <= endPage; i++) {
      if (i === window.jobsCurrentPage) {
        html += `<button class="page-btn current" aria-current="page">${i}</button>`;
      } else {
        html += `<button class="page-btn" data-page="${i}">${i}</button>`;
      }
    }

    if (window.jobsCurrentPage < totalPages) {
      html += `<button class="page-btn" data-page="${window.jobsCurrentPage + 1}" aria-label="Next">Next ›</button>`;
    }

    html += `
      <select id="pageSizeSelect" class="page-size-select" aria-label="Page size">
        <option value="10" ${window.jobsPageSize === 10 ? 'selected' : ''}>10 per page</option>
        <option value="20" ${window.jobsPageSize === 20 ? 'selected' : ''}>20 per page</option>
        <option value="50" ${window.jobsPageSize === 50 ? 'selected' : ''}>50 per page</option>
        <option value="100" ${window.jobsPageSize === 100 ? 'selected' : ''}>100 per page</option>
      </select>
    `;

    html += '</div>';
    container.innerHTML = html;

    container.querySelectorAll('.page-btn[data-page]').forEach(btn => {
      btn.addEventListener('click', () => {
        window.jobsCurrentPage = parseInt(btn.dataset.page, 10);
        fetchJobs();
      });
    });

    const pageSizeSelect = document.getElementById('pageSizeSelect');
    if (pageSizeSelect) {
      pageSizeSelect.addEventListener('change', (e) => {
        window.jobsPageSize = parseInt(e.target.value, 10);
        window.jobsCurrentPage = 1;
        fetchJobs();
      });
    }
  }