const COLORS = { edit: "#2D49D2", run: "#0F7C66", restore: "#A87A0C", fork: "#93379A" };
const HEIGHT = { edit: 64, run: 26, restore: 44, fork: 64 };

export default function Tally({ marks }) {
  return (
    <div className="tally">
      {marks.map((m, i) => <span key={i} title={m} style={{ background: COLORS[m], height: HEIGHT[m] }} />)}
    </div>
  );
}
