\\ specs/mux.shen
\\ Executable pure protocol authority for shenmux.
\\ The host owns PTY, ZeroMQ, terminal FFI, clocks, and persistence. These
\\ definitions own legal session transitions. Screen deltas, control changes,
\\ and exit share one monotonic sequence.

(datatype client-id
  Id : string;
  (not (empty? Id)) : verified;
  ==============================
  Id : client-id;)

(datatype seq-no
  N : number;
  (>= N 0) : verified;
  =====================
  N : seq-no;)

(datatype dimensions
  Cols : number;
  Rows : number;
  (> Cols 0) : verified;
  (> Rows 0) : verified;
  ================================
  [Cols Rows] : dimensions;)

\\ Snapshot payload is a bounded canonical screen checkpoint plus a bounded
\\ interpreted transition tail. It never contains raw PTY control strings.
(datatype snapshot
  Seq : seq-no;
  Dim : dimensions;
  CursorX : number;
  CursorY : number;
  AltScreen : boolean;
  Payload : string;
  (>= CursorX 0) : verified;
  (>= CursorY 0) : verified;
  ============================================================
  [Seq Dim CursorX CursorY AltScreen Payload] : snapshot;)

\
\ Constructor predicates are named functions so generated host constructors
\ execute the same rules rather than re-encoding datatype premises.
(define mux.valid-client-id?
  {string --> boolean}
  Id -> (not (= Id "")))

(define mux.valid-dimensions?
  {number --> number --> boolean}
  Cols Rows -> (and (> Cols 0) (> Rows 0)))

(define mux.valid-snapshot-meta?
  {number --> number --> number --> number --> number --> boolean}
  Seq Cols Rows CursorX CursorY ->
    (and (>= Seq 0)
         (and (mux.valid-dimensions? Cols Rows)
              (and (>= CursorX 0)
                   (and (< CursorX Cols)
                        (and (>= CursorY 0)
                             (< CursorY Rows)))))))

\ [Clients LastSeq LastSnap Dim WriterLocked Exited Controller]
\\ Controller is [] for no owner or [Cid] for exactly one owner.
(datatype session
  Clients : (list client-id);
  LastSeq : seq-no;
  LastSnap : snapshot;
  Dim : dimensions;
  WriterLocked : boolean;
  Exited : boolean;
  Controller : (list client-id);
  ==============================================================================
  [Clients LastSeq LastSnap Dim WriterLocked Exited Controller] : session;)

(define mux.member?
  {A --> (list A) --> boolean}
  _ [] -> false
  X [X | _] -> true
  X [_ | Rest] -> (mux.member? X Rest))

(define mux.remove-client
  {client-id --> (list client-id) --> (list client-id)}
  _ [] -> []
  C [C | Rest] -> Rest
  C [X | Rest] -> [X | (mux.remove-client C Rest)])

(define mux.release-if-owner
  {client-id --> (list client-id) --> (list client-id)}
  C [C] -> []
  _ Owner -> Owner)

(define mux.attach-ok?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked _ _] C ->
    (and (not Locked)
         (not (mux.member? C Clients))))

(define mux.has-control?
  {session --> client-id --> boolean}
  [_ _ _ _ _ _ Controller] C ->
    (= Controller [C]))

(define mux.accept-input?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked Exited Controller] C ->
    (and (not Locked)
         (and (not Exited)
              (and (mux.member? C Clients)
                   (= Controller [C])))))

(define mux.accept-resize?
  {session --> client-id --> boolean}
  S C -> (mux.accept-input? S C))

(define mux.acquire-control-ok?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked Exited Controller] C ->
    (and (not Locked)
         (and (not Exited)
              (and (mux.member? C Clients)
                   (or (= Controller [])
                       (= Controller [C]))))))

(define mux.release-control-ok?
  {session --> client-id --> boolean}
  S C -> (mux.has-control? S C))

(define mux.begin-snapshot
  {session --> session}
  [Clients Seq Snap Dim _ Exited Controller] ->
    [Clients Seq Snap Dim true Exited Controller])

(define mux.end-snapshot
  {session --> snapshot --> session}
  [Clients Seq _ Dim _ Exited Controller] NewSnap ->
    [Clients Seq NewSnap Dim false Exited Controller])

(define mux.attach
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Controller] C ->
    [[C | Clients] Seq Snap Dim Locked Exited Controller])

(define mux.detach
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Controller] C ->
    [(mux.remove-client C Clients)
     Seq Snap Dim Locked Exited (mux.release-if-owner C Controller)])

(define mux.acquire-control
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited _] C ->
    [Clients Seq Snap Dim Locked Exited [C]])

(define mux.release-control
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Controller] C ->
    [Clients Seq Snap Dim Locked Exited (mux.release-if-owner C Controller)])

(define mux.next-seq
  {seq-no --> seq-no}
  N -> (+ N 1))

(define mux.event-ok?
  {session --> seq-no --> boolean}
  [_ Current _ _ Locked _ _] Candidate ->
    (and (not Locked)
         (= Candidate (mux.next-seq Current))))

(define mux.apply-delta
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked Exited Controller] Seq ->
    [Clients Seq Snap Dim Locked Exited Controller])

(define mux.apply-resize
  {session --> seq-no --> dimensions --> session}
  [Clients _ Snap _ Locked Exited Controller] Seq Dim ->
    [Clients Seq Snap Dim Locked Exited Controller])

(define mux.apply-control
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked Exited Controller] Seq ->
    [Clients Seq Snap Dim Locked Exited Controller])

(define mux.apply-exit
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked _ Controller] Seq ->
    [Clients Seq Snap Dim Locked true Controller])
