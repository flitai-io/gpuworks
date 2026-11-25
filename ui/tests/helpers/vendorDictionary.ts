// Preloaded into every test process (package.json passes it to tsx with
// --import). The console takes its vendors from the server's dictionary, so
// a test starts with the dictionary the server sends: the fixture is the
// response's wire JSON, written by the Go test of that endpoint
// (TestAcceleratorVendorsWireJSONIsTheConsoleFixture). A test of the empty
// or failed dictionary sets it itself.
import fs from "node:fs";

import { setVendorDictionary } from "../../src/vendors.ts";

const fixture = JSON.parse(fs.readFileSync(new URL("../fixtures/accelerator-vendors.json", import.meta.url), "utf8"));
setVendorDictionary(fixture.items);
