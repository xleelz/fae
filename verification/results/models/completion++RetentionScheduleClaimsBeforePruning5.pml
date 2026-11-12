// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go#L5
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
	chan child_RetentionScheduleClaimsBeforePruning50 = [1] of {int}->
	run RetentionScheduleClaimsBeforePruning5(child_RetentionScheduleClaimsBeforePruning50)->
	child_RetentionScheduleClaimsBeforePruning50?0->
stop_process:skip
}

proctype RetentionScheduleClaimsBeforePruning5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousRetentionScheduleClaimsBeforePruning1373 = [1] of {int}->
	chan child_AnonymousRetentionScheduleClaimsBeforePruning1272 = [1] of {int}->
	chan child_AnonymousRetentionScheduleClaimsBeforePruning1161 = [1] of {int}->
	chan child_AnonymousRetentionScheduleClaimsBeforePruning1070 = [1] of {int}->
	Chandef pruned_ch->
	Chandef planned_ch->
	Chandef claimed_ch->
	Chandef authorized_ch->
	run sync_monitor(authorized_ch)->
	run sync_monitor(claimed_ch)->
	run sync_monitor(planned_ch)->
	run sync_monitor(pruned_ch)->
	run AnonymousRetentionScheduleClaimsBeforePruning107(claimed_ch,planned_ch,pruned_ch,authorized_ch,child_AnonymousRetentionScheduleClaimsBeforePruning1070)->
	run receiver(child_AnonymousRetentionScheduleClaimsBeforePruning1070)->
	run AnonymousRetentionScheduleClaimsBeforePruning116(authorized_ch,claimed_ch,planned_ch,pruned_ch,child_AnonymousRetentionScheduleClaimsBeforePruning1161)->
	run receiver(child_AnonymousRetentionScheduleClaimsBeforePruning1161)->
	run AnonymousRetentionScheduleClaimsBeforePruning127(claimed_ch,planned_ch,pruned_ch,authorized_ch,child_AnonymousRetentionScheduleClaimsBeforePruning1272)->
	run receiver(child_AnonymousRetentionScheduleClaimsBeforePruning1272)->
	run AnonymousRetentionScheduleClaimsBeforePruning137(claimed_ch,planned_ch,pruned_ch,authorized_ch,child_AnonymousRetentionScheduleClaimsBeforePruning1373)->
	run receiver(child_AnonymousRetentionScheduleClaimsBeforePruning1373)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:14:2 */
	:: pruned_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:14:2 */->
	:: pruned_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:14:2 */ -> 
		pruned_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRetentionScheduleClaimsBeforePruning107(Chandef claimed_ch;Chandef planned_ch;Chandef pruned_ch;Chandef authorized_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:10:14 */
	:: authorized_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:10:14 */
	:: authorized_ch.sync!false -> 
		authorized_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:10:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRetentionScheduleClaimsBeforePruning116(Chandef authorized_ch;Chandef claimed_ch;Chandef planned_ch;Chandef pruned_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:11:14 */
	:: authorized_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:11:14 */->
	:: authorized_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:11:14 */ -> 
		authorized_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:11:28 */
	:: claimed_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:11:28 */
	:: claimed_ch.sync!false -> 
		claimed_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:11:28 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRetentionScheduleClaimsBeforePruning127(Chandef claimed_ch;Chandef planned_ch;Chandef pruned_ch;Chandef authorized_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:12:14 */
	:: claimed_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:12:14 */->
	:: claimed_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:12:14 */ -> 
		claimed_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:12:25 */
	:: planned_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:12:25 */
	:: planned_ch.sync!false -> 
		planned_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:12:25 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRetentionScheduleClaimsBeforePruning137(Chandef claimed_ch;Chandef planned_ch;Chandef pruned_ch;Chandef authorized_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:13:14 */
	:: planned_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:13:14 */->
	:: planned_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:13:14 */ -> 
		planned_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:13:25 */
	:: pruned_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:13:25 */
	:: pruned_ch.sync!false -> 
		pruned_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/retention_schedule.go:13:25 */
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

