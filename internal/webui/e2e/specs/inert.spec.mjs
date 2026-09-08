// Hostile text is shown and is inert - and the grader that says so is defeated on purpose.
import { test, expect } from "@playwright/test";
import { mutate } from "./conventions.mjs";
import { gradeHostileTextIsInert, readHostileTextPair } from "./inert.mjs";

test("a hostile path, reason and bucket are rendered as inert text", async ({ browser, baseURL }) => {
  const { base, got, s, refusals } = await readHostileTextPair(browser, baseURL);
  const probs = gradeHostileTextIsInert(base, got, s, refusals);
  expect(probs, probs.join("\n")).toEqual([]);
});

// F11. The markup a hostile media path carries turned into a real element and a real
// event-handler attribute, which is the failure the inert-text grader exists to refuse: the
// path arrives as things the browser will act on instead of as characters on screen.
//
// The counterexample is built with DOM calls rather than by re-parsing the text, and it is
// built deliberately so that NEITHER half reaches the network: DOMParser is itself a
// Trusted Types sink under this page's own policy, and an image with a src is refused by
// `default-src 'none'`, so either route would have made this case red for a POLICY reason -
// which is the policy grader's property and has its own counterexample. What this one has
// to defeat is inertness, so the element it adds fetches nothing.
test("the inert-text grader fails against hostile text turned into markup", async ({ browser, baseURL }) => {
  const mutation = mutate.script(`
setInterval(function () {
  var td = document.querySelector("#queue td.path");
  if (!td || td.dataset.mutated === "1") return;
  var text = td.textContent;
  if (text.indexOf("<img") < 0) return;
  td.dataset.mutated = "1";
  td.appendChild(document.createElement("img"));
  var handler = /\\son(\\w+)=/.exec(text);
  if (handler) td.setAttribute("on" + handler[1], "void 0");
}, 20);`);
  const { base, got, s, refusals } = await readHostileTextPair(browser, baseURL, mutation);
  expect(got.media + got.handlers,
    `the mutation introduced neither an element nor a handler attribute (media ${got.media}, handlers ${got.handlers}), so this case measured nothing`
  ).toBeGreaterThan(0);
  const probs = gradeHostileTextIsInert(base, got, s, refusals);
  expect(probs.length,
    `the grader PASSED a page that turned a hostile media path into markup (${got.media} media elements, ${got.handlers} handler attributes)`
  ).toBeGreaterThan(0);
});
