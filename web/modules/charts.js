// Forge Dashboard — Chart.js Rendering
import { METRIC } from './constants.js';

let chartJobStatus = null;
let chartStepDuration = null;
let chartTokens = null;
let sparklineCharts = {}; // statName -> Chart instance

export function getChartInstances() {
  return { chartJobStatus, chartStepDuration, chartTokens, sparklineCharts };
}

export function setChartInstances(charts) {
  chartJobStatus = charts.chartJobStatus;
  chartStepDuration = charts.chartStepDuration;
  chartTokens = charts.chartTokens;
  sparklineCharts = charts.sparklineCharts;
}

export function renderCharts(parsedMetrics, sumMetricValAny, getMetricValAny) {
  if (typeof Chart === 'undefined') {
    console.warn('Chart.js library not loaded');
    return;
  }

  renderStatusChart(parsedMetrics, sumMetricValAny);
  renderStepDurationChart(parsedMetrics);
  renderTokensChart(parsedMetrics);
}

export function renderStatusChart(parsedMetrics, sumMetricValAny) {
  const canvas = document.getElementById('chartJobStatus');
  if (!canvas) return;

  const completed = sumMetricValAny(parsedMetrics, METRIC.jobsCompleted, METRIC.workerJobsCompleted);
  const failed = sumMetricValAny(parsedMetrics, METRIC.jobsFailed, METRIC.workerJobsFailed);
  const pending = sumMetricValAny(parsedMetrics, METRIC.pendingJobs, METRIC.workerInFlightJobs);

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

export function renderStepDurationChart(parsedMetrics) {
  const canvas = document.getElementById('chartStepDuration');
  if (!canvas) return;

  // Step duration metrics are in forge_worker_* namespace
  const histogram = parsedMetrics[METRIC.workerStepDuration + '_bucket'] || parsedMetrics['forge_worker_step_duration_seconds_bucket'] || [];
  const labels = [];
  const cumulativeValues = [];

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

export function renderTokensChart(parsedMetrics) {
  const canvas = document.getElementById('chartTokens');
  if (!canvas) return;

  const tokenMetrics = parsedMetrics[METRIC.workerLLMTokens] || [];
  const backendMap = new Map();

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
    renderSingleBackendChart(canvas, 0, 0);
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
  // Implementation omitted for brevity - would create a stacked bar for single backend
}