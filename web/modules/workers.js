// Forge Dashboard — Worker Fleet Management
import { parsePrometheusText } from './metricsParser.js';

let workerList = [];
let workerMetricsCache = {};

export function getWorkerList() {
  return workerList;
}

export function setWorkerList(list) {
  workerList = list;
}

export async function fetchWorkers() {
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

export async function fetchWorkerMetrics(workerName) {
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

export async function fetchWorkerMetricsRaw(workerName) {
  try {
    const res = await fetch(`/api/worker-metrics/${encodeURIComponent(workerName)}`);
    if (!res.ok) throw new Error('Offline');
    const text = await res.text();
    return parsePrometheusText(text);
  } catch (err) {
    return {};
  }
}

export async function fetchAllWorkerMetrics() {
  const { workerList } = await import('./workers.js'); // Circular import workaround
  if (!workerList || workerList.length === 0) return;

  const promises = workerList.map(w => fetchWorkerMetricsRaw(w));
  const results = await Promise.all(promises);
  results.forEach((metrics, i) => {
    workerMetricsCache[workerList[i]] = metrics;
  });
}

export function getWorkerMetricsCache() {
  return workerMetricsCache;
}