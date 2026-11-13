import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { createInstance } from "i18next";
import { productMessageText } from "../src/apiError.ts";

for (const language of ["en", "zh"]) {
 test(`runtime failures localize stable facts in ${language} and retain unknown fallbacks`, async () => {
  const common = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/common.json`, import.meta.url), "utf8"));
  const i18n = createInstance();
  await i18n.init({lng:language, resources:{[language]:{common}},defaultNS:"common",interpolation:{escapeValue:false}});
  const t=i18n.getFixedT(language,"common");
  for (const code of ["ACCELERATOR_DEVICE_SELECTION_REQUIRED","ACCELERATOR_RUNTIME_UNSUPPORTED","ACCELERATOR_RUNTIME_UNREPORTED","ACCELERATOR_DEVICE_UNAVAILABLE","ACCELERATOR_INVENTORY_UNAVAILABLE","ACCELERATOR_RESOURCE_UNAVAILABLE"]) {
   const text=productMessageText(t,{code,params:{vendor:"Kunlunxin",os:"linux",arch:"arm64",runtime:"host",required_os:"linux",required_arch:"amd64",required_runtime:"docker",resource:"memlock"},message:"old English diagnostic"});
   assert.notEqual(text,"old English diagnostic");
   assert.doesNotMatch(text,/ACCELERATOR_|\{\{/);
   assert.match(text,/Kunlunxin/);
   if(code==="ACCELERATOR_RESOURCE_UNAVAILABLE")assert.match(text,/memlock/);
   if(code==="ACCELERATOR_RUNTIME_UNSUPPORTED")assert.match(text,/arm64/);
  }
  assert.equal(productMessageText(t,{code:"FUTURE_DRIVER_ERROR",message:"future diagnostic"}),"future diagnostic");
 });
}
