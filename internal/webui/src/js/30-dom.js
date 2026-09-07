// The DOM primitives. Everything the page puts on screen is built here or through here,
// and there is exactly one way to do it: createElement plus textContent. No module in
// this directory assigns a string to an HTML sink, which is what makes the response
// Content-Security-Policy's `require-trusted-types-for 'script'` a guarantee rather than
// a hope - a regression that string-built from an attacker-influencable media path would
// throw in the browser instead of silently reintroducing a sink.
const $ = (id) => document.getElementById(id);

// mk(tag, class, text): the one DOM-node factory. textContent means any string it is
// handed - a media path, a failure reason - is inert text, never parsed as markup.
function mk(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}
// The honest "not recorded" node for a nil/absent outcome field.
function nrNode() { return mk("span", "nr", NOT_RECORDED); }
