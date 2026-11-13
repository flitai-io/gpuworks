import { getInstanceByDom } from "echarts/core";

export type BenchmarkVisualFormat = "png" | "html";

// Capture the rendered report, including ECharts canvases, without a second
// report template or a browser-side rendering dependency.
async function captureBenchmarkReport(root: HTMLElement, format: BenchmarkVisualFormat) {
	const doc = root.ownerDocument;
	const view = doc.defaultView!;
	await doc.fonts?.ready;
	for (const host of root.querySelectorAll<HTMLElement>("[_echarts_instance_]")) {
		const chart = getInstanceByDom(host);
		chart?.resize();
		chart?.getZr().flush();
	}
	await new Promise<void>((resolve) => view.requestAnimationFrame(() => view.requestAnimationFrame(() => resolve())));
	const width = Math.ceil(root.getBoundingClientRect().width);
	const height = Math.ceil(root.scrollHeight);
	if (!width || !height) throw new Error("The report is not visible");
	if (format === "png" && (Math.max(width, height) > 16384 || width * height > 32_000_000)) {
		throw new Error("The report is too large for a single image");
	}
	const clone = root.cloneNode(true) as HTMLElement;
	const originals = [root, ...root.querySelectorAll("*")];
	const copies = [clone, ...clone.querySelectorAll("*")];
	const pseudoStyles: string[] = [];
	for (let i = 0; i < originals.length; i++) {
		const original = originals[i];
		const copy = copies[i] as HTMLElement | SVGElement;
		if (original.closest("[data-report-exclude], script, link, iframe, object, embed")) {
			copy.remove();
			continue;
		}
		for (const attribute of [...copy.attributes]) {
			if (/^on/i.test(attribute.name) || ["href", "srcset", "action", "formaction"].includes(attribute.name)) copy.removeAttribute(attribute.name);
		}
		copy.setAttribute("style", inlineStyle(view.getComputedStyle(original)));
		for (const pseudo of ["::before", "::after"]) {
			const style = view.getComputedStyle(original, pseudo);
			if (style.content && style.content !== "none" && style.content !== "normal") {
				copy.setAttribute("data-image-node", String(i));
				pseudoStyles.push(`[data-image-node="${i}"]${pseudo}{${inlineStyle(style)}}`);
			}
		}
		if (original instanceof view.HTMLCanvasElement) {
			const image = doc.createElement("img");
			image.src = original.toDataURL("image/png");
			image.setAttribute("style", copy.getAttribute("style")!);
			copy.replaceWith(image);
		}
		if (original instanceof view.HTMLImageElement) {
			if (!original.complete || !original.naturalWidth) await original.decode();
			const canvas = doc.createElement("canvas");
			canvas.width = original.naturalWidth;
			canvas.height = original.naturalHeight;
			const context = canvas.getContext("2d");
			if (!context) throw new Error("Image export is unavailable in this browser");
			context.drawImage(original, 0, 0);
			copy.setAttribute("src", canvas.toDataURL("image/png"));
		}
	}
	clone.style.margin = "0";
	clone.style.width = `${width}px`;
	clone.style.height = `${height}px`;
	clone.style.background = "#f5f7fa";
	clone.style.position = "relative";
	clone.style.inset = "auto";
	// Refuse missing images rather than producing an incomplete offline report.
	if ([...clone.querySelectorAll("img")].some((img) => !img.src.startsWith("data:image/png"))) {
		throw new Error("The report contains an unsupported external image");
	}
	const style = doc.createElement("style");
	style.textContent = pseudoStyles.join("\n");
	clone.prepend(style);
	return { clone, width, height };
}

export async function renderBenchmarkReport(root: HTMLElement, format: BenchmarkVisualFormat): Promise<Blob> {
	const { clone, width, height } = await captureBenchmarkReport(root, format);
	const doc = root.ownerDocument;
	const view = doc.defaultView!;
	if (format === "html") {
		const report = doc.implementation.createHTMLDocument(root.querySelector("h2")?.textContent || "GPUWorks");
		report.documentElement.lang = doc.documentElement.lang;
		const charset = report.createElement("meta");
		charset.setAttribute("charset", "utf-8");
		report.head.prepend(charset);
		const policy = report.createElement("meta");
		policy.httpEquiv = "Content-Security-Policy";
		policy.content = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'";
		report.head.append(policy);
		// Many cells share the same computed style. Store each style once to keep
		// the standalone file smaller without dropping visual properties.
		const styles = new Map<string, string>();
		for (const element of [clone, ...clone.querySelectorAll("[style]")]) {
			const css = element.getAttribute("style")!;
			if (!styles.has(css)) styles.set(css, `report-style-${styles.size}`);
			element.classList.add(styles.get(css)!);
			element.removeAttribute("style");
		}
		const stylesheet = report.createElement("style");
		stylesheet.textContent = [...styles].map(([css, name]) => `.${name}{${css}}`).join("\n");
		report.head.append(stylesheet);
		report.body.style.cssText = "margin:0;padding:24px;background:#f5f7fa;";
		report.body.append(clone);
		return new Blob(["<!doctype html>", report.documentElement.outerHTML], { type: "text/html;charset=utf-8" });
	}
	// Bound memory and canvas dimensions on browsers with smaller canvas limits.
	const scale = Math.min(2, 16384 / Math.max(width, height), Math.sqrt(32_000_000 / (width * height)));
	const markup = new view.XMLSerializer().serializeToString(clone);
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}"><foreignObject width="100%" height="100%">${markup}</foreignObject></svg>`;
	const image = new view.Image();
	image.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`;
	await image.decode();
	const canvas = doc.createElement("canvas");
	canvas.width = Math.round(width * scale);
	canvas.height = Math.round(height * scale);
	const context = canvas.getContext("2d");
	if (!context) throw new Error("Image export is unavailable in this browser");
	context.scale(scale, scale);
	context.drawImage(image, 0, 0);
	return new Promise((resolve, reject) => canvas.toBlob((blob) => blob ? resolve(blob) : reject(new Error("Image export failed")), "image/png"));
}

function inlineStyle(style: CSSStyleDeclaration) {
	return Array.from(style).map((property) => `${property}:${style.getPropertyValue(property)};`).join("") + "animation:none!important;transition:none!important;";
}

export function downloadBenchmarkReport(blob: Blob, runID: number, format: BenchmarkVisualFormat) {
	const url = URL.createObjectURL(blob);
	const link = document.createElement("a");
	link.href = url;
	link.download = `benchmark-${runID}.${format}`;
	document.body.append(link);
	link.click();
	link.remove();
	// Give the browser time to begin consuming the object URL.
	setTimeout(() => URL.revokeObjectURL(url), 1000);
}
