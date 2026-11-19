export const followThreshold = 80;

export const nearBottom = (scrollTop: number, clientHeight: number, scrollHeight: number, threshold = followThreshold): boolean =>
  scrollHeight - scrollTop - clientHeight <= threshold;
