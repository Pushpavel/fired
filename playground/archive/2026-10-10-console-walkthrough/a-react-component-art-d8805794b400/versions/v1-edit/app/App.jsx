import { useState } from "react";
import Tally from "./Tally.jsx";
import "./app.css";

export default function App() {
  const [marks, setMarks] = useState(["edit", "run", "run"]);
  return (
    <main>
      <h1>Tally</h1>
      <p>Each button adds a mark. Edit this file and publish to change the component.</p>
      <div className="buttons">
        {["edit", "run", "restore", "fork"].map(k => (
          <button key={k} onClick={() => setMarks([...marks, k])}>{k}</button>
        ))}
      </div>
      <Tally marks={marks} />
    </main>
  );
}
