import { useCallback, useEffect, useLayoutEffect, useRef, useState, type RefObject } from "react";
import { nearBottom } from "../lib/scroll";

interface Input {
  root: RefObject<HTMLDivElement | null>;
  composer: RefObject<HTMLDivElement | null>;
  transcript: RefObject<HTMLDivElement | null>;
  onTranscript: boolean;
  onReport: boolean;
  focus?: string;
  onFocus: (target: string | undefined) => void;
  turns: unknown;
  events: unknown;
  partialText: unknown;
  toolOutput: unknown;
  mine: unknown;
  loading: boolean;
}

const toBottom = () => window.scrollTo({ top: document.documentElement.scrollHeight });

export function useFollowScroll({ root, composer, transcript, onTranscript, onReport, focus, onFocus, turns, events, partialText, toolOutput, mine, loading }: Input) {
  const [behind, setBehind] = useState(false);
  const following = useRef(true);

  // The user's own scrolling decides whether new content is followed, so scrolling up is never fought.
  useEffect(() => {
    const onScroll = () => {
      const el = document.documentElement;
      following.current = nearBottom(el.scrollTop, el.clientHeight, el.scrollHeight);
      if (following.current) setBehind(false);
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);

  // Not following keeps the arriving content from pulling the view away from the target.
  const jump = useCallback(
    (target: string) => {
      following.current = false;
      onFocus(target);
    },
    [onFocus],
  );

  // The URL drops the target once applied, so a reload doesn't jump again.
  useEffect(() => {
    if (!focus || onReport) return;
    const el = document.getElementById(focus);
    if (!el) return;
    following.current = false;
    const reveal = () => el.scrollIntoView({ block: "start" });
    reveal();
    // Turns off screen are laid out lazily, so the first scroll can land short.
    requestAnimationFrame(() => requestAnimationFrame(reveal));
    el.classList.remove("flash");
    void el.offsetWidth;
    el.classList.add("flash");
    setTimeout(() => el.classList.remove("flash"), 1500);
    onFocus(undefined);
  }, [focus, onReport, turns, onFocus]);

  useLayoutEffect(() => {
    if (!onTranscript) return;
    if (following.current) toBottom();
    else setBehind(true);
  }, [events, partialText, toolOutput, mine, loading, onTranscript]);

  // The transcript's bottom padding tracks the composer, and content that grows after render (calls settling, markdown) is still followed.
  useLayoutEffect(() => {
    const el = composer.current;
    const host = root.current;
    if (!el || !host) return;
    const measure = () => {
      host.style.setProperty("--composer-h", `${el.offsetHeight}px`);
      if (following.current) toBottom();
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    if (transcript.current) ro.observe(transcript.current);
    return () => {
      ro.disconnect();
      host.style.removeProperty("--composer-h");
    };
  }, [onTranscript]);

  const jumpToLatest = () => {
    following.current = true;
    setBehind(false);
    toBottom();
  };

  return { following, behind, jump, jumpToLatest };
}
