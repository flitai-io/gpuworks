// Self-contained so Playwright can serialize this function into the page.
export function tableLinkFindings() {
  const visible = el => {
    const rect = el.getBoundingClientRect();
    const style = getComputedStyle(el);
    return rect.width > 0 && rect.height > 0 && style.visibility !== "hidden" && Number(style.opacity) > 0.05;
  };
  const dialogs = [...document.querySelectorAll('[role="dialog"]')].filter(visible);
  const root = dialogs.at(-1) ?? document.querySelector('.app-shell') ?? document.body;
  const findings = [];
  const candidates = root.querySelectorAll('td a[href], td.resource-table-name button, button.hm-node-name, .page-header-breadcrumb a[href]');
  for (const el of candidates) {
    if (!visible(el) || el.closest('[aria-disabled="true"], [disabled]') || el.querySelector('button')) continue;
    if (el.tagName === 'A' && el.matches('.ant-btn, [data-slot="button"], [role="button"]')) continue;
    // Icons and copy/menu controls are not resource-name text.
    if (el.tagName === 'BUTTON' && !el.textContent.trim()) continue;
    const probe = document.createElement('span');
    probe.style.color = 'var(--fg-brand)';
    el.append(probe);
    const brand = getComputedStyle(probe).color;
    // Legacy antd text links use the primary brand token; Zeron uses fg-brand.
    // They differ in dark mode but are both intentional theme colors.
    probe.style.color = 'var(--brand)';
    const allowed = [brand, getComputedStyle(probe).color];
    if (el.matches(':hover, :active, :focus-visible')) {
      for (const token of ['--brand-hover', '--brand-active']) {
        probe.style.color = `var(${token})`;
        allowed.push(getComputedStyle(probe).color);
      }
    }
    probe.remove();
    const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
    let mismatch;
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const parent = node.parentElement;
      if (!node.textContent.trim() || parent.closest('svg, [aria-hidden="true"], .sr-only') || !visible(parent)) continue;
      const color = getComputedStyle(parent).color;
      if (!allowed.includes(color)) { mismatch = color; break; }
    }
    if (mismatch) {
      const box = el.getBoundingClientRect();
      findings.push({ rule: el.closest('.page-header-breadcrumb') ? 'breadcrumb-link-color' : 'table-link-color', element: el.textContent.trim().slice(0, 60), detail: `${mismatch}; expected brand ${brand}`, box: { x: box.x, y: box.y, w: box.width, h: box.height } });
    }
  }
  return findings;
}
