import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { defineConfig } from "vite";

const notices = ["echarts/NOTICE", "echarts/LICENSE", "echarts/licenses/LICENSE-d3", "zrender/LICENSE", "tslib/LICENSE.txt"]
	.map(path => readFileSync(new URL(`./node_modules/${path}`, import.meta.url), "utf8")).join("\n");

export default defineConfig({
	publicDir: false,
	plugins: [{
		name: "offline-script-integrity",
		writeBundle({ dir }) {
			const script = resolve(dir!, "benchmark-report.js");
			const source = readFileSync(script, "utf8").replace(/<\/script/gi, "<\\/script");
			writeFileSync(script, source);
			writeFileSync(resolve(dir!, "benchmark-report.sha256.txt"), createHash("sha256").update(source).digest("base64"));
		},
	}],
	define: { "process.env.NODE_ENV": '"production"' },
	build: {
		outDir: "node_modules/.cache/gpuworks-report",
		emptyOutDir: false,
		rollupOptions: { output: { banner: `/*!\n${notices}\n*/` } },
		lib: { entry: "src/pages/evaluations/standaloneCharts.ts", name: "BenchmarkReport", formats: ["iife"], fileName: () => "benchmark-report.js" },
	},
});
