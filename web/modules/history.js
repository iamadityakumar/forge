// Forge Dashboard — Metric History (circular buffers for sparklines)
import { SPARKLINE_HISTORY_MAX } from './constants.js';

const metricHistory = {};

export function addToHistory(metricName, value) {
  if (!metricHistory[metricName]) {
    metricHistory[metricName] = [];
  }
  metricHistory[metricName].push({ time: Date.now(), value });
  if (metricHistory[metricName].length > SPARKLINE_HISTORY_MAX) {
    metricHistory[metricName].shift();
  }
}

export function getHistory(metricName) {
  return metricHistory[metricName] || [];
}

export function clearHistory() {
  Object.keys(metricHistory).forEach(key => delete metricHistory[key]);
}