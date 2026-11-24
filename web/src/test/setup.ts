import globalJsdom from "global-jsdom";
import { afterEach } from "bun:test";

// Not happy-dom: its TreeWalker.previousNode() breaks React Aria arrow keys (capricorn86/happy-dom#2152).
globalJsdom();
// Testing Library binds `screen` to document when first imported, so it loads only after the DOM exists.
const { cleanup } = await import("@testing-library/react");
afterEach(cleanup);
