// crypto.randomUUID exists only in secure contexts, and the server may be reached over plain HTTP on a LAN address.
export function randomId(): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
}
