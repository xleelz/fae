// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go#L3
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_ReviewMergeAfterIndependentGates30 = [1] of {int}->
	run ReviewMergeAfterIndependentGates3(child_ReviewMergeAfterIndependentGates30)->
	child_ReviewMergeAfterIndependentGates30?0->
stop_process:skip
}

proctype ReviewMergeAfterIndependentGates3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousReviewMergeAfterIndependentGates1143 = [1] of {int}->
	chan child_AnonymousReviewMergeAfterIndependentGates1072 = [1] of {int}->
	chan child_AnonymousReviewMergeAfterIndependentGates941 = [1] of {int}->
	chan child_AnonymousReviewMergeAfterIndependentGates840 = [1] of {int}->
	Chandef merged_ch->
	Chandef published_ch->
	Chandef comments_ch->
	Chandef checks_ch->
	run sync_monitor(checks_ch)->
	run sync_monitor(comments_ch)->
	run sync_monitor(published_ch)->
	run sync_monitor(merged_ch)->
	run AnonymousReviewMergeAfterIndependentGates84(checks_ch,comments_ch,published_ch,merged_ch,child_AnonymousReviewMergeAfterIndependentGates840)->
	run receiver(child_AnonymousReviewMergeAfterIndependentGates840)->
	run AnonymousReviewMergeAfterIndependentGates94(checks_ch,comments_ch,published_ch,merged_ch,child_AnonymousReviewMergeAfterIndependentGates941)->
	run receiver(child_AnonymousReviewMergeAfterIndependentGates941)->
	run AnonymousReviewMergeAfterIndependentGates107(merged_ch,checks_ch,comments_ch,published_ch,child_AnonymousReviewMergeAfterIndependentGates1072)->
	run receiver(child_AnonymousReviewMergeAfterIndependentGates1072)->
	run AnonymousReviewMergeAfterIndependentGates114(checks_ch,comments_ch,published_ch,merged_ch,child_AnonymousReviewMergeAfterIndependentGates1143)->
	run receiver(child_AnonymousReviewMergeAfterIndependentGates1143)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:12:2 */
	:: merged_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:12:2 */->
	:: merged_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:12:2 */ -> 
		merged_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReviewMergeAfterIndependentGates84(Chandef checks_ch;Chandef comments_ch;Chandef published_ch;Chandef merged_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:8:13 */
	:: checks_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:8:13 */
	:: checks_ch.sync!false -> 
		checks_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:8:13 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReviewMergeAfterIndependentGates94(Chandef checks_ch;Chandef comments_ch;Chandef published_ch;Chandef merged_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:9:13 */
	:: comments_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:9:13 */
	:: comments_ch.sync!false -> 
		comments_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:9:13 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReviewMergeAfterIndependentGates107(Chandef merged_ch;Chandef checks_ch;Chandef comments_ch;Chandef published_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:10:13 */
	:: checks_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:10:13 */->
	:: checks_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:10:13 */ -> 
		checks_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:10:23 */
	:: published_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:10:23 */
	:: published_ch.sync!false -> 
		published_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:10:23 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReviewMergeAfterIndependentGates114(Chandef checks_ch;Chandef comments_ch;Chandef published_ch;Chandef merged_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:13 */
	:: published_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:13 */->
	:: published_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:13 */ -> 
		published_ch.rcving!false
	fi->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:26 */
	:: comments_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:26 */->
	:: comments_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:26 */ -> 
		comments_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:38 */
	:: merged_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:38 */
	:: merged_ch.sync!false -> 
		merged_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/review.go:11:38 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

