// Forge Dashboard — Stats & Sparklines
import { METRIC, THRESHOLDS } from './constants.js';
import { addToHistory, getHistory } from './history.js';

const statConfig = [
  { id: 'statSubmitted', metric: METRIC.jobsSubmitted, fallback: METRIC.workerJobsCompleted, sparklineId: 'sparklineSubmitted', color: '#10b981' },
  { id: 'statCompleted', metric: METRIC.jobsCompleted, fallback: METRIC.workerJobsCompleted, sparklineId: 'sparklineCompleted', color: '#10b981' },
  { id: 'statFailed', metric: METRIC.jobsFailed, fallback: METRIC.workerJobsFailed, sparklineId: 'sparklineFailed', color: '#ef4444' },
  { id: 'statPending', metric: METRIC.pendingJobs, fallback: METRIC.workerInFlightJobs, sparklineId: 'sparklinePending', color: '#f59e0b' },
  { id: 'statWorkers', metric: METRIC.activeWorkers, fallback: METRIC.workerClaims, sparklineId: 'sparklineWorkers', color: '#6366f1' },
  { id: 'statWaits', metric: METRIC.rateLimitWaits, fallback: METRIC.workerRateLimitWaits, sparklineId: 'sparklineWaits', color: '#8b5cf6' },
};

export function updateStats(parsedMetrics, sumMetricValAny, getMetricValAny, addToHistory, getHistory) {
  statConfig.forEach(({ id, metric, fallback, sparklineId, color }) => {
    const val = sumMetricValAny(metric, fallback);
    const el = document.getElementById(id);
    if (el) {
      el.textContent = val;

      // Add to history for sparklines
      addToHistory(metric, val);

      // Update threshold indicators
      updateThresholdIndicator(el, metric, val);
    }
  });

  renderSparklines();
}

export function updateThresholdIndicator(element, metricName, value) {
  const thresholds = THRESHOLDS[metricName];
  if (!thresholds) return;

  element.classList.remove('threshold-warning', 'threshold-critical');

  if (metricName === 'activeWorkers') {
    if (value <= thresholds.critical) {
      element.classList.add('threshold-critical');
    } else if (value <= thresholds.warning) {
      element.classList.add('threshold-warning');
    }
  } else {
    if (value >= thresholds.critical) {
      element.classList.add('threshold-critical');
    } else if (value >= thresholds.warning) {
      element.classList.add('threshold-warning');
    }
  }
}

function renderSparklines() {
  const sparklineConfig = [
    { canvasId: 'sparklineSubmitted', metric: 'forge_api_jobs_submitted_total', fallback: 'forge_worker_jobs_completed_total', color: '#10b981' },
    { canvasId: 'sparklineCompleted', metric: 'forge_api_jobs_completed_total', fallback: 'forge_worker_jobs_completed_total', color: '#10b981' },
    { canvasId: 'sparklineFailed', metric: 'forge_api_jobs_failed_total', fallback: 'forge_worker_jobs_failed_total', color: '#ef4444' },
    { canvasId: 'sparklinePending', metric: 'forge_api_pending_jobs', fallback: 'forge_worker_in_flight_jobs', color: '#f59e0b' },
    { canvasId: 'sparklineWorkers', metric: 'forge_api_active_workers', fallback: 'forge_worker_claims_total', color: '#6366f1' },
    { canvasId: 'sparklineWaits', metric: 'forge_api_rate_limit_waits_total', fallback: 'forge_worker_rate_limit_waits_total', color: '#8b5cf6' },
  ];

  sparklineConfig.forEach(({ canvasId, metric, fallback, color }) => {
    const canvas = document.getElementById(canvasId);
    if (!canvas) return;

    const history = getHistory(metric);
    if (history.length === 0) {
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

  const last = history[history.length - 1];
  const x = padding + ((history.length - 1) / (history.length - 1)) * (width - 2 * padding);
  const y = height - padding - ((last.value - min) / (max - min || 1)) * (height - 2 * padding);
  ctx.beginPath();
  ctx.arc(x, y, 3, 0, Math.PI * 2);
  ctx.fillStyle = color;
  ctx.fill();
}