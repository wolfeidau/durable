import { Code, ConnectError, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import { getToken } from "../lib/token";

const bearer: Interceptor = (next) => (req) => {
  req.header.set("Authorization", `Bearer ${getToken()}`);
  return next(req);
};
export const transport = createConnectTransport({ baseUrl: location.origin, interceptors: [bearer] });

// The client lives outside React, so the auth flag goes through a listener.
let listener = () => {};
export function onUnauthenticated(fn: () => void): () => void {
  listener = fn;
  return () => {
    listener = () => {};
  };
}
const flagAuth = (err: unknown) => {
  if (ConnectError.from(err).code === Code.Unauthenticated) listener();
};
export const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError: flagAuth }),
  mutationCache: new MutationCache({ onError: flagAuth }),
});
