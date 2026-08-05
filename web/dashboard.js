// Forge Dashboard Engine — High-Performance Chart.js Observability
(function () {
  let selectedJobId = null;
  let pollInterval = 5000;
  let traceInterval = 2000;
  let parsedMetrics = {};
  let workerList = [];

  // Chart.js instance references
  let chartJobStatus = null;
  let chartStepDuration = null;
  let chartTokens = null;

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

  // Fetch Health
  async function fetchHealth() {
    try {
      const res = await fetch('/health');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();

      document.getElementById('dbStatus').textContent = data.db || 'ok';
      document.getElementById('activeWorkersCount').textContent = data.workers_online ?? 0;
      document.getElementById('uptimeText').textContent = formatUptime(data.uptime_seconds || 0);

      const dot = document.getElementById('statusDot');
      const text = document.getElementById('statusText');
      if (data.status === 'ok') {
        dot.className = 'pulse-dot';
        text.textContent = 'System Healthy';
      } else {
        dot.className = 'pulse-dot degraded';
        text.textContent = 'System Degraded';
      }
    } catch (err) {
      document.getElementById('statusDot').className = 'pulse-dot offline';
      document.getElementById('statusText').textContent = 'System Unreachable';
    }
  }

  // Fetch API Metrics
  async function fetchMetrics() {
    try {
      const res = await fetch('/metrics');
      if (!res.ok) return;
      const text = await res.text();
      parsedMetrics = parsePrometheusText(text);

      updateStats();
      renderCharts();
    } catch (err) {
      console.error('Failed to fetch metrics:', err);
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

  function updateStats() {
    document.getElementById('statSubmitted').textContent = sumMetricVal('forge_jobs_submitted_total');
    document.getElementById('statCompleted').textContent = sumMetricVal('forge_jobs_completed_total');
    document.getElementById('statFailed').textContent = sumMetricVal('forge_jobs_failed_total');
    document.getElementById('statPending').textContent = getMetricVal('forge_pending_jobs');
    document.getElementById('statWorkers').textContent = getMetricVal('forge_active_workers');
    document.getElementById('statWaits').textContent = sumMetricVal('forge_rate_limit_waits_total');
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

      const claims = wMetrics['forge_claims_total'] ? wMetrics['forge_claims_total'][0].value : 0;
      const comp = wMetrics['forge_jobs_completed_total'] ? wMetrics['forge_jobs_completed_total'][0].value : 0;

      if (claimsEl) claimsEl.textContent = claims;
      if (compEl) compEl.textContent = comp;
    } catch (err) {
      if (badge) {
        badge.className = 'worker-badge badge-offline';
        badge.textContent = 'OFFLINE';
      }
    }
  }

  // Fetch Jobs List
  async function fetchJobs() {
    try {
      const res = await fetch('/jobs');
      if (!res.ok) return;
      const jobs = await res.json();

      const tbody = document.getElementById('jobsTableBody');
      if (!jobs || jobs.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" class="empty-state">No jobs found</td></tr>';
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
    } catch (err) {
      console.error('Failed to fetch jobs:', err);
    }
  }

  function selectJob(id) {
    selectedJobId = id;
    document.getElementById('selectedJobId').textContent = id;
    document.getElementById('timelineContainer').style.display = 'block';
    fetchSelectedTrace();
    fetchJobs(); // Update row selection styling
  }

  async function fetchSelectedTrace() {
    if (!selectedJobId) return;
    try {
      const res = await fetch(`/jobs/${selectedJobId}/trace`);
      if (!res.ok) return;
      const steps = await res.json();

      const timeline = document.getElementById('stepTimeline');
      const reclaimNotice = document.getElementById('reclaimNotice');

      if (!steps || steps.length === 0) {
        timeline.innerHTML = '<div class="empty-state">No steps recorded yet for this job</div>';
        if (reclaimNotice) reclaimNotice.style.display = 'none';
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
    } catch (err) {
      console.error('Failed to fetch trace:', err);
    }
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

    const completed = sumMetricVal('forge_jobs_completed_total');
    const failed = sumMetricVal('forge_jobs_failed_total');
    const pending = getMetricVal('forge_pending_jobs');

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

    const histogram = parsedMetrics['forge_step_duration_seconds_bucket'] || [];
    const labels = [];
    const dataValues = [];

    for (let item of histogram) {
      if (item.labels.le) {
        const lbl = item.labels.le === '+Inf' ? '+Inf' : `≤${item.labels.le}s`;
        labels.push(lbl);
        dataValues.push(item.value);
      }
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

    const promptTokens = sumMetricValWithLabel('forge_llm_tokens_total', { kind: 'prompt' });
    const compTokens = sumMetricValWithLabel('forge_llm_tokens_total', { kind: 'completion' });

    const labels = ['Tokens'];

    if (chartTokens) {
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