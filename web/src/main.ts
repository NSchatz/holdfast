import { mount } from "svelte";

import App from "./App.svelte";
import "./app.css";

const target = document.getElementById("app");
if (target === null) {
  throw new Error("index.html has no #app element to mount the UI in");
}

mount(App, { target });
