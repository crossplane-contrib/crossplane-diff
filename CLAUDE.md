- * we should never silently continue a diff in the face of a failure, and we should strive not to make best-guesses. 
* the diff command should strive for accuracy above all else, so in cases where errors or guesses may compromise accuracy, we should fail the diff completely.
* we should not emit partial results for a given XR; if given multiple XRs, it's okay to emit results only for those that pass so long as we call attention to the others that failed.
* we should always emit useful logging with appropriate contextual objects attached.