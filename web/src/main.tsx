import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider } from "@tanstack/react-query";
import { createRoot } from "react-dom/client";
import { App } from "./shell/App";
import { queryClient, transport } from "./shell/client";
import "./app.css";

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
