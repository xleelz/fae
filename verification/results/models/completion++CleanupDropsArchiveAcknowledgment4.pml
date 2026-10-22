// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go#L4
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
	chan child_CleanupDropsArchiveAcknowledgment40 = [1] of {int}->
	run CleanupDropsArchiveAcknowledgment4(child_CleanupDropsArchiveAcknowledgment40)->
	child_CleanupDropsArchiveAcknowledgment40?0->
stop_process:skip
}

proctype CleanupDropsArchiveAcknowledgment4(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousCleanupDropsArchiveAcknowledgment1072 = [1] of {int}->
	chan child_AnonymousCleanupDropsArchiveAcknowledgment951 = [1] of {int}->
	chan child_AnonymousCleanupDropsArchiveAcknowledgment860 = [1] of {int}->
	Chandef removed_ch->
	Chandef archive_ch->
	Chandef backup_ch->
	run sync_monitor(backup_ch)->
	run sync_monitor(archive_ch)->
	run sync_monitor(removed_ch)->
	run AnonymousCleanupDropsArchiveAcknowledgment86(archive_ch,removed_ch,backup_ch,child_AnonymousCleanupDropsArchiveAcknowledgment860)->
	run receiver(child_AnonymousCleanupDropsArchiveAcknowledgment860)->
	run AnonymousCleanupDropsArchiveAcknowledgment95(backup_ch,archive_ch,removed_ch,child_AnonymousCleanupDropsArchiveAcknowledgment951)->
	run receiver(child_AnonymousCleanupDropsArchiveAcknowledgment951)->
	run AnonymousCleanupDropsArchiveAcknowledgment107(removed_ch,backup_ch,archive_ch,child_AnonymousCleanupDropsArchiveAcknowledgment1072)->
	run receiver(child_AnonymousCleanupDropsArchiveAcknowledgment1072)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:11:2 */
	:: removed_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:11:2 */->
	:: removed_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:11:2 */ -> 
		removed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCleanupDropsArchiveAcknowledgment86(Chandef archive_ch;Chandef removed_ch;Chandef backup_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:8:13 */
	:: backup_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:8:13 */
	:: backup_ch.sync!false -> 
		backup_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:8:13 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCleanupDropsArchiveAcknowledgment95(Chandef backup_ch;Chandef archive_ch;Chandef removed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:9:13 */
	:: archive_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:9:13 */
	:: archive_ch.sync!false -> 
		archive_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:9:13 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousCleanupDropsArchiveAcknowledgment107(Chandef removed_ch;Chandef backup_ch;Chandef archive_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:10:13 */
	:: backup_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:10:13 */->
	:: backup_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:10:13 */ -> 
		backup_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:10:23 */
	:: removed_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:10:23 */
	:: removed_ch.sync!false -> 
		removed_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/cleanup.go:10:23 */
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

