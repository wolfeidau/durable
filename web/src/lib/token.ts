import { readStored, store } from "./storage";

const tokenKey = "durable.token";

function initialToken(): string {
  const fromHash = new URLSearchParams(location.hash.slice(1)).get("token");
  if (!fromHash) return readStored(tokenKey);
  store(tokenKey, fromHash);
  // Keep the token out of history and the address bar.
  history.replaceState(null, "", location.pathname + location.search);
  return fromHash;
}

let token = initialToken();

export const getToken = (): string => token;

export function setToken(t: string): void {
  token = t;
  store(tokenKey, t);
}
