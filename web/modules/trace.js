// Forge Dashboard — Trace & LLM Calls
import { JAEGER_BASE_URL } from './constants.js';

let selectedJobId = null;
let selectedJobTraceContext = null;

export function getSelectedJobId() {
  return selectedJobId;
}

export function setSelectedJobId(id) {
  selectedJobId = id;
}

export function getSelectedJobTraceContext() {
  return selectedJobTraceContext;
}

export function setSelectedJobTraceContext(ctx) {
  selectedJobTraceContext = ctx;
}

export async function fetchSelectedTrace(selectedJobId, updateJaegerLink) {
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
    } catch (err) {
      console.error('Failed to fetch trace:', err);
    }
  }

export async function fetchJobDetails(id) {
  try {
    const res = await fetch(`/jobs/${id}`);
    if (res.ok) {
      const job = await res.json();
      return job.trace_context || null;
    }
  } catch (err) {
    console.error('Failed to fetch job details:', err);
  }
  return null;
}

export function updateJaegerLink(traceContext) {
  const linkEl = document.getElementById('jaegerLink');
  if (!linkEl) return;
  if (traceContext && traceContext.trace_id) {
    const traceId = traceContext.trace_id;
    const jaegerUrl = `http://localhost:16686/trace/${traceId}`;
    linkEl.href = jaegerUrl;
    linkEl.style.display = 'inline-flex';
    linkEl.textContent = `🔗 View in Jaeger (${traceId.substring(0, 16)}...)`;
  } else {
    linkEl.style.display = 'none';
  }
}

export function renderLLMCalls(calls, container) {
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