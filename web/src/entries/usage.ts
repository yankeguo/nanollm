// Usage page charts. The series is server-rendered into an inert JSON block
// (CSP-safe: no inline script execution needed). Colors track the Tailwind
// palette used by the surrounding UI (zinc chrome, sky/amber/emerald series).
import Chart from "chart.js/auto";

interface UsageChartData {
  labels: string[];
  calls: number[];
  input: number[];
  output: number[];
  cache: number[];
  first_token: number[];
  output_speed: number[];
}

// zinc-500 ticks, zinc-800 grid, series in sky/amber/emerald/indigo 300-400.
const tick = { color: "#71717a" };
const grid = { color: "#27272a" };
const tooltip = {
  backgroundColor: "#18181b", // zinc-900
  borderColor: "#3f3f46", // zinc-700
  borderWidth: 1,
  titleColor: "#e4e4e7", // zinc-200
  bodyColor: "#a1a1aa", // zinc-400
  padding: 10,
  cornerRadius: 8,
  displayColors: true,
  boxPadding: 4,
};
const legend = {
  labels: { usePointStyle: true, pointStyle: "circle", boxWidth: 6, boxHeight: 6, padding: 16 },
};

Chart.defaults.color = "#71717a";
Chart.defaults.borderColor = "#27272a";

// Swap a chart canvas for its empty-state block when there is nothing to draw.
// A metric series of all zeros (e.g. every call in the window failed) also
// counts as empty.
function renderOrEmpty(canvasId: string, emptyId: string, build: (el: HTMLCanvasElement) => void, series?: number[]) {
  const el = document.getElementById(canvasId) as HTMLCanvasElement | null;
  if (!el) return;
  const holder = document.getElementById("chart-data");
  const data: UsageChartData = JSON.parse(holder?.textContent || "{}");
  const noMetric = series !== undefined && !series.some((v) => v > 0);
  if (!data.labels || data.labels.length === 0 || noMetric) {
    el.parentElement?.classList.add("hidden");
    document.getElementById(emptyId)?.classList.remove("hidden");
    return;
  }
  build(el);
}

const holder = document.getElementById("chart-data");
if (holder) {
  const data: UsageChartData = JSON.parse(holder.textContent || "{}");
  // input totals include cached tokens; derive the non-cached remainder so the
  // stacked bar always adds up to total input (cache-creation tokens, which are
  // excluded from the stored uncached column, end up in this remainder too).
  const uncached = data.labels.map((_, i) => Math.max(0, (data.input[i] || 0) - (data.cache[i] || 0)));

  renderOrEmpty("tokensChart", "tokensEmpty", (el) => {
    new Chart(el, {
      type: "bar",
      data: {
        labels: data.labels,
        datasets: [
          { label: "uncached input", data: uncached, backgroundColor: "#7dd3fc", stack: "input", borderRadius: 3, maxBarThickness: 28 },
          { label: "cached input", data: data.cache, backgroundColor: "#fcd34d", stack: "input", borderRadius: 3, maxBarThickness: 28 },
          { label: "output", data: data.output, backgroundColor: "#6ee7b7", stack: "output", borderRadius: 3, maxBarThickness: 28 },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend, tooltip },
        scales: {
          x: { ticks: tick, grid: { display: false }, stacked: true },
          y: { ticks: tick, grid: grid, beginAtZero: true, stacked: true },
        },
      },
    });
  });

  renderOrEmpty("callsChart", "callsEmpty", (el) => {
    new Chart(el, {
      type: "line",
      data: {
        labels: data.labels,
        datasets: [
          {
            label: "calls",
            data: data.calls,
            borderColor: "#818cf8",
            backgroundColor: "rgb(129 140 248 / 0.12)",
            tension: 0.3,
            fill: true,
            pointRadius: 2,
            pointBackgroundColor: "#818cf8",
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend, tooltip },
        scales: { x: { ticks: tick, grid: { display: false } }, y: { ticks: tick, grid: grid, beginAtZero: true } },
      },
    });
  });

  // 0 marks a bucket with no measurable calls (all failed/canceled); map it to
  // null so the line shows a gap instead of dipping to zero.
  const gaps = (series: number[]) => series.map((v) => (v > 0 ? v : null));

  renderOrEmpty("ttftChart", "ttftEmpty", (el) => {
    new Chart(el, {
      type: "line",
      data: {
        labels: data.labels,
        datasets: [
          {
            label: "avg first token latency (ms)",
            data: gaps(data.first_token),
            borderColor: "#f0abfc",
            backgroundColor: "rgb(240 171 252 / 0.12)",
            tension: 0.3,
            fill: true,
            pointRadius: 2,
            pointBackgroundColor: "#f0abfc",
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend, tooltip },
        scales: { x: { ticks: tick, grid: { display: false } }, y: { ticks: tick, grid: grid, beginAtZero: true } },
      },
    });
  }, data.first_token);

  renderOrEmpty("speedChart", "speedEmpty", (el) => {
    new Chart(el, {
      type: "line",
      data: {
        labels: data.labels,
        datasets: [
          {
            label: "avg output speed (tok/s)",
            data: gaps(data.output_speed),
            borderColor: "#fdba74",
            backgroundColor: "rgb(253 186 116 / 0.12)",
            tension: 0.3,
            fill: true,
            pointRadius: 2,
            pointBackgroundColor: "#fdba74",
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend, tooltip },
        scales: { x: { ticks: tick, grid: { display: false } }, y: { ticks: tick, grid: grid, beginAtZero: true } },
      },
    });
  }, data.output_speed);
}
