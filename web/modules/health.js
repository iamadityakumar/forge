// Forge Dashboard — Health & System Status
export async function fetchHealth() {
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

export function formatUptime(sec) {
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}