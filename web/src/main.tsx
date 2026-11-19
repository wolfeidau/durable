import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { createRoot } from "react-dom/client";
import { App } from "./shell/App";
import { queryClient, transport } from "./shell/client";
// The import order is the cascade order, so components never import CSS.
import "./styles/tokens.css";
import "./styles/base.css";
import "./ui/ui.css";
import "./shell/shell.css";
import "./sessions/sessions.css";
import "./server/server.css";
import "./session/session.css";
import "./session/transcript/transcript.css";
import "./session/report/report.css";
import "./session/network/network.css";
import "./styles/responsive.css";

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </TransportProvider>,
  );
}
