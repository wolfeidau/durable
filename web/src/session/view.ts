import type { Tab } from "../lib/route";

export type View = "transcript" | "report" | "network";

export type OpenTab = (tab: Tab | undefined, call?: string) => void;

// A focus target lives in the transcript, so it wins over the tab in the URL.
export const viewOf = (tab: Tab | undefined, focus: string | undefined): View => (focus ? "transcript" : (tab ?? "transcript"));
