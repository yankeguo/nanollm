// Usage page charts. The series is server-rendered into an inert JSON block
// (CSP-safe: no inline script execution needed).
import Chart from "chart.js/auto";

interface UsageChartData {
  labels: string[];
  calls: number[];
  input: number[];
  output: number[];
  cache: number[];
}

const holder = document.getElementById("chart-data");
if (holder) {
  const data: UsageChartData = JSON.parse(holder.textContent || "{}");
  // input totals include cached tokens; derive the non-cached remainder so the
  // stacked bar always adds up to total input (cache-creation tokens, which are
  // excluded from the stored uncached column, end up in this remainder too).
  const uncached = data.labels.map((_, i) => Math.max(0, (data.input[i] || 0) - (data.cache[i] || 0)));
  const tick = { color: "#8b9aab" };
  const grid = { color: "#2a3644" };
  Chart.defaults.color = "#8b9aab";
  Chart.defaults.borderColor = "#2a3644";

  const tokensEl = document.getElementById("tokensChart") as HTMLCanvasElement | null;
  if (tokensEl) {
    new Chart(tokensEl, {
      type: "bar",
      data: {
        labels: data.labels,
        datasets: [
          { label: "uncached input", data: uncached, backgroundColor: "#5b9fd4", stack: "input" },
          { label: "cached input", data: data.cache, backgroundColor: "#c9a45c", stack: "input" },
          { label: "output", data: data.output, backgroundColor: "#6dbe8a", stack: "output" },
        ],
      },
      options: {
        responsive: true,
        scales: {
          x: { ticks: tick, grid: grid, stacked: true },
          y: { ticks: tick, grid: grid, beginAtZero: true, stacked: true },
        },
      },
    });
  }

  const callsEl = document.getElementById("callsChart") as HTMLCanvasElement | null;
  if (callsEl) {
    new Chart(callsEl, {
      type: "line",
      data: {
        labels: data.labels,
        datasets: [{ label: "calls", data: data.calls, borderColor: "#5b9fd4", tension: 0.2, fill: false }],
      },
      options: {
        responsive: true,
        scales: { x: { ticks: tick, grid: grid }, y: { ticks: tick, grid: grid, beginAtZero: true } },
      },
    });
  }
}
