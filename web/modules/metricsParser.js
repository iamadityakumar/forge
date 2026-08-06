// Forge Dashboard — Prometheus Metrics Parser
export function parsePrometheusText(text) {
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

export function getMetricVal(parsedMetrics, name, matchLabels = {}) {
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

export function sumMetricVal(parsedMetrics, name) {
  const list = parsedMetrics[name];
  if (!list) return 0;
  return list.reduce((acc, curr) => acc + curr.value, 0);
}

export function sumMetricValWithLabel(parsedMetrics, name, matchLabels) {
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

export function getMetricValAny(parsedMetrics, orchestratorName, workerName, matchLabels = {}) {
  let val = getMetricVal(parsedMetrics, orchestratorName, matchLabels);
  if (val > 0) return val;
  return sumMetricVal(parsedMetrics, workerName);
}

export function sumMetricValAny(parsedMetrics, orchestratorName, workerName) {
  let val = sumMetricVal(parsedMetrics, orchestratorName);
  if (val > 0) return val;
  return sumMetricVal(parsedMetrics, workerName);
}

export function sumMetricValWithLabelAny(parsedMetrics, orchestratorName, workerName, matchLabels) {
  let val = sumMetricValWithLabel(parsedMetrics, orchestratorName, matchLabels);
  if (val > 0) return val;
  return sumMetricValWithLabel(parsedMetrics, workerName, matchLabels);
}