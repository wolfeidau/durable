const tokenKey = "durable.token";

function readStored(): string {
  try {
    return localStorage.getItem(tokenKey) ?? "";
  } catch {
    return "";
  }
}

function store(token: string): void {
  try {
    localStorage.setItem(tokenKey, token);
  } catch {
    // Storage can be blocked; the token then lasts for this page only.
  }
}

function initialToken(): string {
  const fromHash = new URLSearchParams(location.hash.slice(1)).get("token");
  if (!fromHash) return readStored();
  store(fromHash);
  // Keep the token out of history and the address bar.
  history.replaceState(null, "", location.pathname + location.search);
  return fromHash;
}

let token = initialToken();

export const getToken = (): string => token;

export function setToken(t: string): void {
  token = t;
  store(t);
}
