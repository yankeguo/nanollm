// Rewrite server-rendered UTC <time datetime> stamps into the browser's
// timezone. The header range filter shows quick ranges plus custom from/to
// wall times in the selected zone (default: browser zone); on submit the
// wall times are converted back to UTC instants. Without JS everything
// falls back to UTC.

let browserTZ = "";
try {
  browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
} catch {
  // no Intl timezone support; stay on UTC
}

const opts: Intl.DateTimeFormatOptions = {
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
};
const noSec = new Intl.DateTimeFormat(undefined, opts);
const withSec = new Intl.DateTimeFormat(undefined, { ...opts, second: "2-digit" });

document.querySelectorAll<HTMLTimeElement>("time[datetime]").forEach((el) => {
  const d = new Date(el.getAttribute("datetime") || "");
  if (isNaN(d.getTime())) return;
  el.textContent = ("min" in el.dataset ? noSec : withSec).format(d);
  el.title = d.toString();
});

if (browserTZ) {
  document.querySelectorAll("[data-tz]").forEach((el) => {
    el.textContent = browserTZ;
  });
}

function wallParts(d: Date, zone: string): Record<string, string> {
  const p: Record<string, string> = {};
  new Intl.DateTimeFormat("en-CA", {
    timeZone: zone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hourCycle: "h23",
  })
    .formatToParts(d)
    .forEach((x) => {
      p[x.type] = x.value;
    });
  return p;
}

function wallValue(d: Date, zone: string): string {
  const p = wallParts(d, zone);
  return `${p.year}-${p.month}-${p.day}T${p.hour}:${p.minute}`;
}

// Interpret "YYYY-MM-DDTHH:MM" as wall time in zone and return the instant.
function instantFromWall(val: string, zone: string): Date | null {
  const m = /^(\d+)-(\d+)-(\d+)T(\d+):(\d+)/.exec(val);
  if (!m) return null;
  const target = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5]);
  let guess = target;
  for (let i = 0; i < 3; i++) {
    const p = wallParts(new Date(guess), zone);
    const diff = target - Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour, +p.minute);
    if (!diff) break;
    guess += diff;
  }
  return new Date(guess);
}

document.querySelectorAll<HTMLFormElement>("form.rangefilter").forEach((form) => {
  const rangeSel = form.querySelector<HTMLSelectElement>("select[name=range]");
  const tzSel = form.querySelector<HTMLSelectElement>("select[name=tz]");
  const dts = form.querySelectorAll<HTMLInputElement>("input[type=datetime-local]");
  let zones: string[] = [];
  try {
    zones = (Intl as any).supportedValuesOf?.("timeZone") || [];
  } catch {
    // old engine; the server-rendered option stays
  }
  if (tzSel) {
    if (zones.indexOf("UTC") < 0) zones.unshift("UTC");
    let cur = tzSel.value;
    if (!tzSel.hasAttribute("data-explicit") && browserTZ) cur = browserTZ;
    if (cur && zones.indexOf(cur) < 0) zones.unshift(cur);
    tzSel.innerHTML = "";
    zones.forEach((z) => {
      const o = document.createElement("option");
      o.value = z;
      o.textContent = z;
      if (z === cur) o.selected = true;
      tzSel.appendChild(o);
    });
    if (!tzSel.value) tzSel.value = "UTC";
  }
  const zone = () => (tzSel && tzSel.value) || "UTC";
  const refresh = () => {
    dts.forEach((el) => {
      const raw = el.getAttribute("data-utc");
      if (!raw) return;
      const d = new Date(raw);
      if (isNaN(d.getTime())) return;
      el.value = wallValue(d, zone());
    });
  };
  refresh();
  if (tzSel) tzSel.addEventListener("change", refresh);
  // The custom-range controls (from/to, tz, Apply) only make sense for a
  // custom range: hide and disable them otherwise so quick ranges submit
  // clean URLs. Quick ranges submit on change; custom only reveals them.
  const bits = form.querySelector(".custombits");
  const syncBits = () => {
    const custom = rangeSel && rangeSel.value === "custom";
    if (bits) bits.classList.toggle("hidden", !custom);
    dts.forEach((el) => {
      el.disabled = !custom;
    });
    if (tzSel) tzSel.disabled = !custom;
  };
  syncBits();
  if (rangeSel)
    rangeSel.addEventListener("change", () => {
      syncBits();
      if (rangeSel.value !== "custom") form.submit();
    });
  form.addEventListener("submit", () => {
    dts.forEach((el) => {
      if (!el.value) return;
      const d = instantFromWall(el.value, zone());
      if (!d || isNaN(d.getTime())) return;
      const hid = document.createElement("input");
      hid.type = "hidden";
      hid.name = el.name;
      hid.value = d.toISOString();
      el.disabled = true;
      form.appendChild(hid);
    });
  });
});

// Copy buttons: any [data-copy] copies the text of its previous sibling
// (the <pre>/<code> it decorates) and flashes a check icon on success.
// Buttons stay hidden where the async clipboard API is unavailable.
{
  const canCopy = !!(navigator.clipboard && window.isSecureContext !== false);
  document.querySelectorAll<HTMLElement>("[data-copy]").forEach((btn) => {
    if (!canCopy) {
      btn.classList.add("hidden");
      return;
    }
    btn.addEventListener("click", async (e) => {
      e.stopPropagation();
      const src = btn.previousElementSibling;
      if (!src || !src.textContent) return;
      try {
        await navigator.clipboard.writeText(src.textContent);
      } catch {
        return;
      }
      const icon = btn.querySelector("[class*='icon-[']");
      if (!icon) return;
      const prev = icon.getAttribute("class") || "";
      icon.setAttribute("class", prev.replace(/icon-\[lucide--[\w-]+\]/, "icon-[lucide--check]"));
      setTimeout(() => icon.setAttribute("class", prev), 1200);
    });
  });
}

// Rows with data-href (calls table) navigate on click; interactive elements
// inside the row keep their own behavior, and modifier-clicks open a new tab.
document.addEventListener("click", (e) => {
  const t = e.target as Element | null;
  if (!t || t.closest("a,button,select,input,textarea,label")) return;
  const row = t.closest("tr[data-href]") as HTMLElement | null;
  if (!row) return;
  const href = row.dataset.href;
  if (!href) return;
  if (e.metaKey || e.ctrlKey) {
    window.open(href, "_blank");
  } else {
    window.location.href = href;
  }
});
