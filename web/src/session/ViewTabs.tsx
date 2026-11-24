import type { ReactElement, ReactNode, Ref } from "react";
import { Tab, TabList, TabPanel, Tabs } from "react-aria-components";
import type { OpenTab } from "./transcript/Call";
import type { View } from "./view";


interface Props {
  view: View;
  onTab: OpenTab;
  sandboxed: boolean;
  deniedCount: number;
  actions?: ReactNode;
  children: ReactNode;
  ref?: Ref<HTMLDivElement>;
}

// The tabs and their panels share one Tabs context, so the whole view body lives inside it.
export function ViewTabs({ view, onTab, sandboxed, deniedCount, actions, children, ref }: Props): ReactElement {
  return (
    <Tabs
      className="session-view"
      ref={ref}
      selectedKey={view}
      // Manual: Report builds its overview and Network fetches, so arrowing past them shouldn't open them.
      keyboardActivation="manual"
      onSelectionChange={(key) => onTab(key === "transcript" ? undefined : (key as "report" | "network"))}
    >
      <div className="toolbar">
        <TabList className="tabs" aria-label="View">
          <Tab id="transcript">Transcript</Tab>
          <Tab id="report">Report</Tab>
          {/* Kept while selected, so Tabs never reselects and rewrites the URL of a sandbox-less session. */}
          {(sandboxed || view === "network") && <Tab id="network">Network{deniedCount > 0 && ` · ${deniedCount} denied`}</Tab>}
        </TabList>
        {actions}
      </div>
      {children}
    </Tabs>
  );
}

// The transcript holds scroll position and watch state, so it stays mounted while another view is selected.
export function ViewPanel({ id, children }: { id: View; children: ReactNode }): ReactElement {
  return (
    <TabPanel id={id} shouldForceMount={id === "transcript"}>
      {children}
    </TabPanel>
  );
}
