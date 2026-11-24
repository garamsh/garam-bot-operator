// Package declaration applies the declarations garam places on a cluster as garam-owned objects of
// garam-bot's interface 1, with one owner per object (ADR 0089, cluster-connection v1alpha1 DECL-1 to
// DECL-15 and CLAIM-1 to CLAIM-3).
//
// It is a pipeline. The feed long-polls garam and keeps the declarations it holds. Each pass reads the
// cluster through the Objects port, decides from the two what to do, carries that out, and keeps what
// came of each declaration for the reports that follow.
package declaration
