// Forge Dashboard — Jobs Management & Pagination
import { parsePrometheusText } from './metricsParser.js';

let jobsCurrentPage = 1;
let jobsPageSize = 20;
let jobsTotalCount = 0;

export function getPaginationState() {
  return { currentPage: jobsCurrentPage, pageSize: jobsPageSize, totalCount: jobsTotalCount };
}

export function setPaginationState({ currentPage, pageSize, totalCount }) {
  if (currentPage !== undefined) jobsCurrentPage = currentPage;
  if (pageSize !== undefined) jobsPageSize = pageSize;
  if (totalCount !== undefined) jobsTotalCount = totalCount;
}

export async function fetchJobs(parsedMetrics, sumMetricValAny, getMetricValAny, addToHistory) {
  try {
    const offset = (jobsCurrentPage - 1) * jobsPageSize;
    const res = await fetch(`/jobs?limit=${jobsPageSize}&offset=${offset}`);
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
  }
}

export function renderPagination() {
  const container = document.getElementById('jobsPagination');
  if (!container) return;

  const totalPages = Math.ceil(jobsTotalCount / jobsPageSize) || 1;
  if (totalPages <= 1) {
    container.innerHTML = '';
    return;
  }

  let html = '<div class="pagination">';

  if (jobsCurrentPage > 1) {
    html += `<button class="page-btn" data-page="${jobsCurrentPage - 1}" aria-label="Previous">‹ Prev</button>`;
  }

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

  if (jobsCurrentPage < totalPages) {
    html += `<button class="page-btn" data-page="${jobsCurrentPage + 1}" aria-label="Next">Next ›</button>`;
  }

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
      jobsCurrentPage = 1;
      fetchJobs();
    });
  }
}

export function getCurrentPage() {
  return jobsCurrentPage;
}

export function getPageSize() {
  return jobsPageSize;
}